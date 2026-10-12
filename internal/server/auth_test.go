package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/limiter"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

// testSecret is the key the test servers accept; do sends it.
const testSecret = "tg_test_key"

// testKey is what testSecret resolves to: limits high enough never to get
// in the way.
var testKey = auth.Key{ID: 1, Name: "test", RPM: 1_000_000, TPM: 1_000_000_000}

// fakeKeys is an Authenticator over a map of secrets.
type fakeKeys struct {
	keys map[string]auth.Key
	err  error // returned for every key, if set
}

func (f *fakeKeys) Authenticate(_ context.Context, presented string) (auth.Key, error) {
	if f.err != nil {
		return auth.Key{}, f.err
	}
	k, ok := f.keys[presented]
	if !ok {
		return auth.Key{}, auth.ErrUnknownKey
	}
	if k.Revoked {
		return auth.Key{}, auth.ErrRevokedKey
	}
	return k, nil
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{keys: map[string]auth.Key{testSecret: testKey}}
}

// fakeLimiter allows everything and records every reservation and refund.
type fakeLimiter struct {
	mu       sync.Mutex
	reserved []int
	refunds  []refund
}

type refund struct{ reserved, used int }

func (f *fakeLimiter) AllowRequest(int64, limiter.Limits) (time.Duration, bool) { return 0, true }

func (f *fakeLimiter) ReserveTokens(_ int64, _ limiter.Limits, n int) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserved = append(f.reserved, n)
	return 0, true
}

func (f *fakeLimiter) RefundTokens(_ int64, _ limiter.Limits, reserved, used int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, refund{reserved, used})
}

func (f *fakeLimiter) snapshot() ([]int, []refund) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.reserved...), append([]refund(nil), f.refunds...)
}

// doAs is do with the given Authorization header ("" sends none).
func doAs(ctx context.Context, s *Server, authorization, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
}

const chatBody = `{"model": "mock-fast", "messages": [{"role": "user", "content": "hi"}]}`

// Acceptance (M4): 401 without a valid key.
func TestUnauthenticatedRequestsGet401(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	keys := s.keys.(*fakeKeys)
	keys.keys["tg_revoked"] = auth.Key{ID: 2, Revoked: true}

	tests := []struct {
		name, authorization, wantMessage string
	}{
		{"no header", "", "No API key provided"},
		{"wrong scheme", "Basic " + testSecret, "No API key provided"},
		{"empty token", "Bearer ", "No API key provided"},
		{"unknown key", "Bearer tg_nope", "Incorrect API key"},
		{"revoked key", "Bearer tg_revoked", "revoked"},
	}
	for _, tt := range tests {
		for _, path := range []string{"/v1/chat/completions", "/v1/models", "/v1/nothing-here"} {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				method := http.MethodPost
				if path != "/v1/chat/completions" {
					method = http.MethodGet
				}
				rec := doAs(t.Context(), s, tt.authorization, method, path, chatBody)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body)
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want Bearer", got)
				}
				e := decodeError(t, rec)
				if deref(e.Code) != api.CodeInvalidAPIKey || !strings.Contains(e.Message, tt.wantMessage) {
					t.Errorf("error = %+v, want invalid_api_key mentioning %q", e, tt.wantMessage)
				}
			})
		}
	}

	// The bearer scheme is case-insensitive, and health checks need no key.
	if rec := doAs(t.Context(), s, "bearer "+testSecret, http.MethodGet, "/v1/models", ""); rec.Code != http.StatusOK {
		t.Errorf("lowercase bearer: status %d, want 200", rec.Code)
	}
	if rec := doAs(t.Context(), s, "", http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz without a key: status %d, want 200", rec.Code)
	}
}

func TestKeyLookupFailureIs503(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	s.keys.(*fakeKeys).err = errors.New("database is down")
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("status = %d, Retry-After %q; want 503 with Retry-After 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if e := decodeError(t, rec); deref(e.Code) != api.CodeAuthUnavailable || strings.Contains(e.Message, "database") {
		t.Errorf("error = %+v, want auth_unavailable without internal detail", e)
	}
}

func TestAllowedAliases(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	s.keys.(*fakeKeys).keys["tg_limited"] = auth.Key{ID: 3, RPM: 100, TPM: 100_000, AllowedModels: []string{"another"}}

	// The alias the key may not use looks exactly like one that doesn't exist.
	notAllowed := doAs(t.Context(), s, "Bearer tg_limited", http.MethodPost, "/v1/chat/completions", chatBody)
	missing := doAs(t.Context(), s, "Bearer tg_limited", http.MethodPost, "/v1/chat/completions",
		`{"model": "no-such", "messages": [{"role": "user", "content": "hi"}]}`)
	for _, rec := range []*httptest.ResponseRecorder{notAllowed, missing} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body)
		}
		if e := decodeError(t, rec); deref(e.Code) != api.CodeModelNotFound ||
			!strings.Contains(e.Message, "does not exist or you do not have access to it") {
			t.Errorf("error = %+v, want model_not_found", e)
		}
	}

	allowed := doAs(t.Context(), s, "Bearer tg_limited", http.MethodPost, "/v1/chat/completions",
		`{"model": "another", "messages": [{"role": "user", "content": "hi"}]}`)
	if allowed.Code != http.StatusOK {
		t.Errorf("allowed alias: status %d, want 200; body %s", allowed.Code, allowed.Body)
	}

	// /v1/models lists only what the key may use.
	models := doAs(t.Context(), s, "Bearer tg_limited", http.MethodGet, "/v1/models", "")
	if body := models.Body.String(); !strings.Contains(body, `"another"`) || strings.Contains(body, `"mock-fast"`) {
		t.Errorf("models for a limited key = %s, want only another", body)
	}
	all := do(t.Context(), s, http.MethodGet, "/v1/models", "")
	if body := all.Body.String(); !strings.Contains(body, `"another"`) || !strings.Contains(body, `"mock-fast"`) {
		t.Errorf("models for an unrestricted key = %s, want both aliases", body)
	}
}

// limitedServer is a test server with the real limiter under a frozen
// clock, and a key with the given limits.
func limitedServer(t *testing.T, p provider.Provider, rpm, tpm int) *Server {
	t.Helper()
	s := testServer(t, p, nil)
	s.limiter = limiter.New(func() time.Time { return fixedTime })
	s.keys.(*fakeKeys).keys[testSecret] = auth.Key{ID: 1, RPM: rpm, TPM: tpm}
	return s
}

func TestRequestsPerMinute(t *testing.T) {
	s := limitedServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), 2, 1_000_000)
	for i := range 2 {
		if rec := do(t.Context(), s, http.MethodGet, "/v1/models", ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i+1, rec.Code)
		}
	}
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("third request: status %d, Retry-After %q; want 429 with Retry-After 30", rec.Code, rec.Header().Get("Retry-After"))
	}
	if e := decodeError(t, rec); e.Type != api.TypeRequests || deref(e.Code) != api.CodeRateLimitExceeded {
		t.Errorf("error = %+v, want type requests, code rate_limit_exceeded", e)
	}
}

func TestTokensPerMinute(t *testing.T) {
	// A request reserves its estimated prompt, 1 token for "hi", plus
	// max_tokens.
	s := limitedServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), 1000, 40)

	// A request that can never fit, 1 + 40 tokens, gets no Retry-After.
	tooBig := do(t.Context(), s, http.MethodPost, "/v1/chat/completions",
		`{"model": "mock-fast", "max_tokens": 40, "messages": [{"role": "user", "content": "hi"}]}`)
	if tooBig.Code != http.StatusTooManyRequests || tooBig.Header().Get("Retry-After") != "" {
		t.Fatalf("too big: status %d, Retry-After %q; want 429 without Retry-After", tooBig.Code, tooBig.Header().Get("Retry-After"))
	}
	if e := decodeError(t, tooBig); e.Type != api.TypeTokens || !strings.Contains(e.Message, "Request too large") {
		t.Errorf("too big: error = %+v", e)
	}

	// Each request reserves the whole limit, 1 + 39, and the refund returns
	// all but the few tokens the mock used. Under a frozen clock nothing
	// refills, so the second request is a few tokens short.
	big := `{"model": "mock-fast", "max_tokens": 39, "messages": [{"role": "user", "content": "hi"}]}`
	first := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", big)
	if first.Code != http.StatusOK {
		t.Fatalf("first: status %d, body %s", first.Code, first.Body)
	}
	second := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", big)
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
		t.Fatalf("second: status %d, Retry-After %q; want 429 with a Retry-After, since the first used some tokens",
			second.Code, second.Header().Get("Retry-After"))
	}
	if e := decodeError(t, second); e.Type != api.TypeTokens || !strings.Contains(e.Message, "Rate limit reached") {
		t.Errorf("second: error = %+v", e)
	}
}
