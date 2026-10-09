package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// sequence is a provider whose calls fail with the given errors in order,
// then succeed. A nil entry is a success; block makes a call wait for its
// context to end.
type sequence struct {
	name  string
	errs  []error
	mu    sync.Mutex
	calls int
	// validate, if set, is the provider's own request check.
	validate func(*provider.ChatRequest) error
}

var errBlock = errors.New("block until cancelled")

func (s *sequence) Name() string { return s.name }

func (s *sequence) next(ctx context.Context) error {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i >= len(s.errs) {
		return nil
	}
	if errors.Is(s.errs[i], errBlock) {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.errs[i]
}

func (s *sequence) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *sequence) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	if err := s.next(ctx); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Model: req.Model, Content: "from " + s.name}, nil
}

func (s *sequence) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.Stream, error) {
	if err := s.next(ctx); err != nil {
		return nil, err
	}
	return &scriptStream{ctx: ctx, steps: []step{content(0, "from "+s.name), {chunk: &provider.Chunk{FinishReason: provider.FinishStop}}}}, nil
}

// withValidate wraps a sequence so it also implements the router's
// validator interface.
type withValidate struct{ *sequence }

func (w withValidate) Validate(req *provider.ChatRequest) error { return w.validate(req) }

func unavailable(name string) error {
	return &provider.Error{Provider: name, Class: provider.Unavailable, Status: 503, Message: "overloaded"}
}

func rateLimited(name string, retryAfter time.Duration) error {
	return &provider.Error{Provider: name, Class: provider.RateLimited, Status: 429, Message: "slow down", RetryAfter: retryAfter}
}

func badRequest(name string) error {
	return &provider.Error{Provider: name, Class: provider.BadRequest, Status: 400, Message: "bad input"}
}

func authError(name string) error {
	return &provider.Error{Provider: name, Class: provider.AuthOrConfig, Status: 401, Message: "bad key"}
}

// twoTargets returns a router whose alias "a" tries a, then b. Each target
// gets 2 attempts; backoff and Retry-After waits follow the given config.
func twoTargets(t *testing.T, a, b provider.Provider, mutate func(*config.Config)) (*Router, *Alias) {
	t.Helper()
	cfg := config.Default()
	cfg.Timeouts = testTimeouts
	cfg.Retry = config.Retry{MaxAttemptsPerTarget: 2, BaseBackoff: 100 * time.Millisecond, MaxBackoff: time.Second, MaxRetryAfter: 2 * time.Second}
	cfg.Models = map[string]config.Model{"a": {DefaultMaxTokens: 10, MaxTokensCeiling: 10, Targets: []config.Target{
		{Provider: a.Name(), Model: "a-1"},
		{Provider: b.Name(), Model: "b-1"},
	}}}
	if mutate != nil {
		mutate(cfg)
	}
	r, err := New(cfg, map[string]provider.Provider{a.Name(): a, b.Name(): b})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := r.Alias("a")
	return r, alias
}

// Acceptance (M3): the router matrix.
func TestRouterMatrix(t *testing.T) {
	tests := []struct {
		name       string
		a, b       []error
		wantServed string // provider that answered, or "" for an error
		wantTries  []string
		wantClass  provider.Class // for an error
		wantWait   time.Duration  // time spent waiting, on the fake clock
	}{
		{
			name: "retry then succeed",
			a:    []error{unavailable("a")}, wantServed: "a",
			wantTries: []string{"unavailable", "ok"},
		},
		{
			name: "fall back after retries",
			a:    []error{unavailable("a"), unavailable("a")}, wantServed: "b",
			wantTries: []string{"unavailable", "unavailable", "ok"},
		},
		{
			name: "bad request is not retried and doesn't fall back",
			a:    []error{badRequest("a")}, wantClass: provider.BadRequest,
			wantTries: []string{"bad_request"},
		},
		{
			name: "short Retry-After is waited out on the same target",
			a:    []error{rateLimited("a", time.Second)}, wantServed: "a",
			wantTries: []string{"rate_limited", "ok"}, wantWait: time.Second,
		},
		{
			name: "long Retry-After falls back at once",
			a:    []error{rateLimited("a", 30*time.Second)}, wantServed: "b",
			wantTries: []string{"rate_limited", "ok"},
		},
		{
			name: "rate limit without Retry-After falls back at once",
			a:    []error{rateLimited("a", 0)}, wantServed: "b",
			wantTries: []string{"rate_limited", "ok"},
		},
		{
			name: "auth or config error falls back without a retry",
			a:    []error{authError("a")}, wantServed: "b",
			wantTries: []string{"auth_or_config", "ok"},
		},
		{
			name: "everything failed",
			a:    []error{unavailable("a"), unavailable("a")}, b: []error{unavailable("b"), unavailable("b")},
			wantClass: provider.Unavailable,
			wantTries: []string{"unavailable", "unavailable", "unavailable", "unavailable"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, b := &sequence{name: "a", errs: tt.a}, &sequence{name: "b", errs: tt.b}
				r, alias := twoTargets(t, a, b, nil)
				r.jitter = func(time.Duration) time.Duration { return 0 } // backoff waits are tested separately

				start := time.Now()
				resp, res, err := r.Chat(t.Context(), alias, provider.ChatRequest{})

				if tt.wantServed != "" {
					if err != nil {
						t.Fatalf("got error %v, want an answer from %s", err, tt.wantServed)
					}
					if resp.Content != "from "+tt.wantServed {
						t.Errorf("answered by %q, want %s", resp.Content, tt.wantServed)
					}
				} else if provider.ClassOf(err) != tt.wantClass {
					t.Errorf("got %v (%v), want class %v", err, provider.ClassOf(err), tt.wantClass)
				}
				got := make([]string, len(res.Tries))
				for i, try := range res.Tries {
					got[i] = try.Outcome
				}
				if !equal(got, tt.wantTries) || res.Attempts != len(tt.wantTries) {
					t.Errorf("tries = %q (%d attempts), want %q", got, res.Attempts, tt.wantTries)
				}
				if waited := time.Since(start); waited != tt.wantWait {
					t.Errorf("waited %v, want %v", waited, tt.wantWait)
				}
			})
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Acceptance (M3): a cancelled request stops at once: no retry, no fallback.
func TestRouterCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := &sequence{name: "a", errs: []error{errBlock}}, &sequence{name: "b"}
		r, alias := twoTargets(t, a, b, nil)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)

		_, res, err := r.Chat(ctx, alias, provider.ChatRequest{})
		if provider.ClassOf(err) != provider.Cancelled {
			t.Errorf("got %v, want Cancelled", err)
		}
		if a.count() != 1 || b.count() != 0 || res.Attempts != 1 {
			t.Errorf("a called %d times, b %d, attempts %d; want 1, 0, 1", a.count(), b.count(), res.Attempts)
		}
	})
}

// Everything failing is an *ExhaustedError, which tells the server which
// status to send.
func TestRouterExhausted(t *testing.T) {
	tests := []struct {
		name             string
		a, b             []error
		wantCapacityOnly bool
		wantRateLimited  bool
		wantRetryAfter   time.Duration
		wantTimeout      bool
	}{
		{
			name: "outright failures: 502",
			a:    []error{unavailable("a"), unavailable("a")}, b: []error{authError("b")},
		},
		{
			name: "every target rate limited: 503 with the shortest wait",
			a:    []error{rateLimited("a", 40*time.Second)}, b: []error{rateLimited("b", 9*time.Second)},
			wantCapacityOnly: true, wantRateLimited: true, wantRetryAfter: 9 * time.Second,
		},
		{
			name: "rate limited and failed: 502",
			a:    []error{rateLimited("a", 40*time.Second)}, b: []error{unavailable("b"), unavailable("b")},
			wantRateLimited: true,
		},
		{
			name:        "out of time: 504",
			a:           []error{errBlock},
			wantTimeout: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, b := &sequence{name: "a", errs: tt.a}, &sequence{name: "b", errs: tt.b}
				r, alias := twoTargets(t, a, b, func(c *config.Config) { c.Timeouts.Total = 10 * time.Second })
				r.jitter = func(time.Duration) time.Duration { return 0 }

				_, _, err := r.Chat(t.Context(), alias, provider.ChatRequest{})
				ex, ok := errors.AsType[*ExhaustedError](err)
				if !ok {
					t.Fatalf("got %v, want an *ExhaustedError", err)
				}
				if ex.CapacityOnly != tt.wantCapacityOnly || ex.RateLimited != tt.wantRateLimited || ex.RetryAfter != tt.wantRetryAfter {
					t.Errorf("capacity only %v, rate limited %v, retry after %v; want %v, %v, %v",
						ex.CapacityOnly, ex.RateLimited, ex.RetryAfter, tt.wantCapacityOnly, tt.wantRateLimited, tt.wantRetryAfter)
				}
				if timedOut := errors.Is(err, ErrTotalTimeout); timedOut != tt.wantTimeout {
					t.Errorf("timed out = %v, want %v (%v)", timedOut, tt.wantTimeout, err)
				}
			})
		})
	}
}

// The breaker opens after repeated failures, the router then skips the
// target without calling it, and after the cool-down one probe gets
// through.
func TestRouterSkipsOpenBreakers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		down := []error{unavailable("a"), unavailable("a"), unavailable("a"), unavailable("a")}
		a, b := &sequence{name: "a", errs: down}, &sequence{name: "b"}
		r, alias := twoTargets(t, a, b, func(c *config.Config) {
			c.Breaker = config.Breaker{FailureThreshold: 3, OpenFor: 30 * time.Second}
		})
		r.jitter = func(time.Duration) time.Duration { return 0 }

		// Request 1: a fails twice (two attempts), b answers.
		if _, res, err := r.Chat(t.Context(), alias, provider.ChatRequest{}); err != nil || res.Provider != "b" {
			t.Fatalf("request 1: %v, served by %s", err, res.Provider)
		}
		// Request 2: a fails once more, which opens its breaker; b answers.
		if _, _, err := r.Chat(t.Context(), alias, provider.ChatRequest{}); err != nil {
			t.Fatal(err)
		}
		// Request 3: a is skipped without a call.
		before := a.count()
		_, res, err := r.Chat(t.Context(), alias, provider.ChatRequest{})
		if err != nil || a.count() != before {
			t.Fatalf("request 3: %v, a called %d more times", err, a.count()-before)
		}
		checkResult(t, res, "b", "b-1", "skipped", "ok")

		// After the cool-down, a gets one probe, which fails (its fourth
		// scripted failure) and reopens the breaker, so its second attempt
		// is refused.
		time.Sleep(30 * time.Second)
		_, res, _ = r.Chat(t.Context(), alias, provider.ChatRequest{})
		checkResult(t, res, "b", "b-1", "unavailable", "skipped", "ok")
		if got := alias.Targets[0].breaker.currentState(); got != open {
			t.Errorf("after a failed probe the breaker is %v, want open", got)
		}
	})
}

func TestRouterEveryBreakerOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := &sequence{name: "a"}, &sequence{name: "b"}
		r, alias := twoTargets(t, a, b, func(c *config.Config) {
			c.Breaker = config.Breaker{FailureThreshold: 1, OpenFor: 30 * time.Second}
		})
		for i := range alias.Targets {
			alias.Targets[i].breaker.record(permit{}, failure)
		}
		time.Sleep(10 * time.Second)

		_, res, err := r.Chat(t.Context(), alias, provider.ChatRequest{})
		ex, ok := errors.AsType[*ExhaustedError](err)
		if !ok || ex.Last != nil || !ex.CapacityOnly || ex.RetryAfter != 20*time.Second {
			t.Errorf("got %+v, want capacity-only with no last error and 20s to wait", ex)
		}
		if a.count()+b.count() != 0 || res.Attempts != 0 {
			t.Error("a target with an open breaker was called")
		}
	})
}

// The backoff before retrying an unavailable target. This test depends on
// the M3 exercise.
func TestRouterWaitsTheBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := &sequence{name: "a", errs: []error{unavailable("a")}}, &sequence{name: "b"}
		r, alias := twoTargets(t, a, b, nil)
		r.jitter = func(d time.Duration) time.Duration { return d } // no jitter: the full cap

		start := time.Now()
		if _, _, err := r.Chat(t.Context(), alias, provider.ChatRequest{}); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 100*time.Millisecond {
			t.Errorf("waited %v before the retry, want the base backoff, 100ms", waited)
		}
	})
}

// Acceptance-adjacent (M3): a request is checked against every target
// before the first attempt.
func TestRouterValidatesEveryTarget(t *testing.T) {
	temp := 1.5
	tests := []struct {
		name      string
		b         provider.Provider
		sampling  *bool
		wantParam string
	}{
		{
			name: "a target without sampling",
			b:    &sequence{name: "b"}, sampling: new(false), wantParam: "temperature",
		},
		{
			name: "a provider's own rule",
			b: withValidate{&sequence{name: "b", validate: func(*provider.ChatRequest) error {
				return &provider.ParamError{Param: "temperature", Message: "must be from 0 to 1"}
			}}},
			wantParam: "temperature",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &sequence{name: "a"}
			r, alias := twoTargets(t, a, tt.b, func(c *config.Config) {
				m := c.Models["a"]
				m.Targets[1].Sampling = tt.sampling
				c.Models["a"] = m
			})
			_, _, err := r.Chat(t.Context(), alias, provider.ChatRequest{Temperature: &temp})
			verr, ok := errors.AsType[*InvalidRequestError](err)
			if !ok || verr.Param != tt.wantParam {
				t.Errorf("got %v, want an InvalidRequestError for %s", err, tt.wantParam)
			}
			if a.count() != 0 {
				t.Error("the first target was called before the request was found invalid")
			}
		})
	}
}

// Streams retry and fall back before the commit point, like Chat.
func TestRouterStreamFallsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &sequence{name: "a", errs: []error{unavailable("a"), errBlock}} // fails, then never answers
		b := &sequence{name: "b"}
		r, alias := twoTargets(t, a, b, func(c *config.Config) { c.Timeouts.FirstToken = 2 * time.Second })
		r.jitter = func(time.Duration) time.Duration { return 0 }

		start := time.Now()
		s, res, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		text, err := collect(s)
		if err != nil || text != "from b" {
			t.Errorf("streamed %q, %v; want the reply from b", text, err)
		}
		checkResult(t, res, "b", "b-1", "unavailable", "unavailable", "ok")
		// a's second attempt hit the first-token timeout, 2s.
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Errorf("fell back after %v, want 2s", elapsed)
		}
	})
}

// A stream that fails after the commit point counts against the breaker.
func TestRouterStreamFailureAfterCommitCountsAgainstTheBreaker(t *testing.T) {
	p := &scripted{steps: []step{content(0, "a"), {err: unavailable("scripted")}}}
	cfg := testConfig(testTimeouts)
	cfg.Breaker = config.Breaker{FailureThreshold: 1, OpenFor: time.Minute}
	cfg.Models = map[string]config.Model{"a": {DefaultMaxTokens: 1, MaxTokensCeiling: 1, Targets: []config.Target{{Provider: "scripted", Model: "m"}}}}
	r, err := New(cfg, map[string]provider.Provider{"scripted": p})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := r.Alias("a")
	s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := collect(s); provider.ClassOf(err) != provider.Unavailable {
		t.Fatalf("got %v, want the mid-stream failure", err)
	}
	if got := alias.Targets[0].breaker.currentState(); got != open {
		t.Errorf("breaker is %v after a failure past the commit point, want open", got)
	}
}
