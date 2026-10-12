package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
	"github.com/ktripathi2281/tollgate/internal/router"
)

// routedServer builds a Server whose alias "mock-fast" tries the given
// targets in order, and captures its logs.
func routedServer(t *testing.T, providers map[string]provider.Provider, targets []config.Target, mutate func(*config.Config)) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.Retry.MaxAttemptsPerTarget = 1
	cfg.Models = map[string]config.Model{"mock-fast": {DefaultMaxTokens: 5, MaxTokensCeiling: 50, Targets: targets}}
	if mutate != nil {
		mutate(cfg)
	}
	r, err := router.New(cfg, providers)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := New(cfg.Server, cfg.Timeouts, r, slog.New(slog.NewJSONHandler(&logs, nil)))
	s.now = func() time.Time { return fixedTime }
	return s, &logs
}

func TestInvalidForATargetIsABadRequest(t *testing.T) {
	providers := map[string]provider.Provider{
		"mock": mock.New("mock", mock.Config{OutputTokens: 1}),
		"pick": mock.New("pick", mock.Config{OutputTokens: 1}),
	}
	// The second target's model rejects sampling parameters.
	s, logs := routedServer(t, providers, []config.Target{
		{Provider: "mock", Model: "m-1"},
		{Provider: "pick", Model: "m-2", Sampling: new(false)},
	}, nil)

	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions",
		`{"model": "mock-fast", "temperature": 0.2, "messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	e := decodeError(t, rec)
	if deref(e.Code) != api.CodeInvalidValue || deref(e.Param) != "temperature" || !strings.Contains(e.Message, "mock-fast") {
		t.Errorf("error = %+v, want invalid_value for temperature naming the model", e)
	}
	// No upstream was called, so there is no upstream story to log.
	if strings.Contains(logs.String(), `"msg":"upstream`) {
		t.Errorf("a rejected request was logged as an upstream outcome:\n%s", logs)
	}
}

func TestEveryBreakerOpenIsA503(t *testing.T) {
	down := mock.New("down", mock.Config{OutputTokens: 1, Status: 503})
	s, _ := routedServer(t, map[string]provider.Provider{"down": down},
		[]config.Target{{Provider: "down", Model: "m"}},
		func(c *config.Config) { c.Breaker = config.Breaker{FailureThreshold: 1, OpenFor: 20 * time.Second} })

	body := `{"model": "mock-fast", "messages": [{"role": "user", "content": "hi"}]}`
	if rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusBadGateway {
		t.Fatalf("first request: status %d, want 502 (the failure that opens the breaker)", rec.Code)
	}
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("with the breaker open: status %d, want 503", rec.Code)
	}
	if e := decodeError(t, rec); deref(e.Code) != api.CodeUpstreamUnavailable {
		t.Errorf("code = %q, want %q", deref(e.Code), api.CodeUpstreamUnavailable)
	}
	// About 20s left on the breaker's cool-down.
	if ra := rec.Header().Get("Retry-After"); ra != "20" {
		t.Errorf("Retry-After = %q, want 20", ra)
	}
	if got := rec.Header().Get(HeaderAttempts); got != "0" {
		t.Errorf("%s = %q, want 0: the target was skipped", HeaderAttempts, got)
	}
}

func TestFallbackAfterAnAuthErrorIsLoggedLoudly(t *testing.T) {
	providers := map[string]provider.Provider{
		"badkey": mock.New("badkey", mock.Config{OutputTokens: 1, Status: 401}),
		"good":   mock.New("good", mock.Config{OutputTokens: 1}),
	}
	s, logs := routedServer(t, providers, []config.Target{
		{Provider: "badkey", Model: "m"},
		{Provider: "good", Model: "m"},
	}, nil)

	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions",
		`{"model": "mock-fast", "messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusOK || rec.Header().Get(HeaderProvider) != "good" || rec.Header().Get(HeaderAttempts) != "2" {
		t.Fatalf("status %d, provider %q, attempts %q; want 200 from good after 2 attempts",
			rec.Code, rec.Header().Get(HeaderProvider), rec.Header().Get(HeaderAttempts))
	}
	for _, want := range []string{`"level":"ERROR"`, `"msg":"upstream recovered"`, `"outcome":"auth_or_config"`, `"target":"good/m"`, rec.Header().Get(HeaderRequestID)} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}
	}
}
