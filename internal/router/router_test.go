package router

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

func newRouter(t *testing.T) *Router {
	t.Helper()
	providers := map[string]provider.Provider{
		"a": mock.New("a", mock.Config{OutputTokens: 3}),
		"b": mock.New("b", mock.Config{OutputTokens: 3}),
	}
	models := map[string]config.Model{
		"fast": {DefaultMaxTokens: 10, MaxTokensCeiling: 20, Targets: []config.Target{{Provider: "b", Model: "b-small"}, {Provider: "a", Model: "a-1"}}},
		"big":  {DefaultMaxTokens: 10, MaxTokensCeiling: 20, Targets: []config.Target{{Provider: "a", Model: "a-2"}}},
	}
	r, err := New(models, providers, testTimeouts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestAliases(t *testing.T) {
	r := newRouter(t)
	if got, want := r.Names(), []string{"big", "fast"}; !slices.Equal(got, want) {
		t.Errorf("Names() = %q, want %q", got, want)
	}
	a, ok := r.Alias("fast")
	if !ok || a.Name != "fast" || a.DefaultMaxTokens != 10 || a.MaxTokensCeiling != 20 || len(a.Targets) != 2 {
		t.Errorf("Alias(fast) = %+v, %v", a, ok)
	}
	if _, ok := r.Alias("missing"); ok {
		t.Error("Alias(missing) found an alias")
	}
}

func TestChatUsesTheFirstTarget(t *testing.T) {
	r := newRouter(t)
	alias, _ := r.Alias("fast")
	req := provider.ChatRequest{
		Model:     "fast",
		Messages:  []provider.Message{{Role: provider.RoleUser, Parts: []string{"hi"}}},
		MaxTokens: 10,
	}

	resp, res, err := r.Chat(t.Context(), alias, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if want := (Result{Provider: "b", Model: "b-small", Attempts: 1}); res != want {
		t.Errorf("Result = %+v, want %+v", res, want)
	}
	if resp.Model != "b-small" {
		t.Errorf("response model = %q, want the upstream model b-small", resp.Model)
	}
	if req.Model != "fast" {
		t.Errorf("the caller's request was changed: Model = %q", req.Model)
	}
}

func TestChatTotalTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hang := mock.New("hang", mock.Config{OutputTokens: 1, Hang: true})
		timeouts := config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: 3 * time.Second}
		r, alias := streamRouter(t, hang, timeouts)

		start := time.Now()
		_, _, err := r.Chat(t.Context(), alias, provider.ChatRequest{})
		if !errors.Is(err, ErrTotalTimeout) || time.Since(start) != 3*time.Second {
			t.Errorf("got %v after %v, want ErrTotalTimeout after 3s", err, time.Since(start))
		}
	})
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestNewRejectsUnknownProvider(t *testing.T) {
	models := map[string]config.Model{"x": {Targets: []config.Target{{Provider: "nope", Model: "m"}}}}
	if _, err := New(models, map[string]provider.Provider{}, testTimeouts); err == nil {
		t.Error("New accepted a target with an unknown provider")
	}
}
