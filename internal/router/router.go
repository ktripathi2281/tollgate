// Package router maps model aliases to upstream targets and calls them:
// within the gateway's timeouts, with retries, fallback to later targets,
// and a circuit breaker per target.
package router

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// Errors for calls ended by the gateway's own timeouts. Each one wraps
// context.DeadlineExceeded, so errors.Is(err, context.DeadlineExceeded)
// holds for all of them and provider.ClassOf treats them as Unavailable.
var (
	ErrFirstTokenTimeout = fmt.Errorf("no output within the first-token timeout: %w", context.DeadlineExceeded)
	ErrIdleTimeout       = fmt.Errorf("no chunk within the idle timeout: %w", context.DeadlineExceeded)
	ErrTotalTimeout      = fmt.Errorf("the request ran past its total timeout: %w", context.DeadlineExceeded)
)

// errBreakerOpen marks a target that was skipped because its breaker is
// open.
var errBreakerOpen = errors.New("circuit breaker open")

// Target is one upstream provider and the model ID to request from it.
type Target struct {
	Provider provider.Provider
	Model    string
	// Sampling says whether the model accepts temperature and top_p.
	Sampling bool
	breaker  *breaker
}

func (t *Target) String() string { return t.Provider.Name() + "/" + t.Model }

// Alias is a model name clients can request.
type Alias struct {
	Name             string
	DefaultMaxTokens int
	MaxTokensCeiling int
	Targets          []Target
}

// Router holds the aliases and the resilience settings. The aliases don't
// change after New, and each breaker guards its own state, so a Router is
// safe for concurrent use.
type Router struct {
	aliases  map[string]*Alias
	names    []string // sorted
	timeouts config.Timeouts
	retry    config.Retry
	// jitter picks the actual wait before a retry; tests replace it.
	jitter func(time.Duration) time.Duration
}

// New builds the aliases in cfg, using the providers built from it. Each
// target gets its own circuit breaker, even when two aliases share a
// provider and model.
func New(cfg *config.Config, providers map[string]provider.Provider) (*Router, error) {
	r := &Router{
		aliases:  make(map[string]*Alias, len(cfg.Models)),
		timeouts: cfg.Timeouts,
		retry:    cfg.Retry,
		jitter:   fullJitter,
	}
	for name, m := range cfg.Models {
		alias := &Alias{Name: name, DefaultMaxTokens: m.DefaultMaxTokens, MaxTokensCeiling: m.MaxTokensCeiling}
		for _, t := range m.Targets {
			p, ok := providers[t.Provider]
			if !ok {
				return nil, fmt.Errorf("alias %s: unknown provider %q", name, t.Provider)
			}
			alias.Targets = append(alias.Targets, Target{
				Provider: p,
				Model:    t.Model,
				Sampling: t.AcceptsSampling(),
				breaker:  newBreaker(cfg.Breaker.FailureThreshold, cfg.Breaker.OpenFor),
			})
		}
		if len(alias.Targets) == 0 {
			return nil, fmt.Errorf("alias %s: no targets", name)
		}
		r.aliases[name] = alias
	}
	r.names = slices.Sorted(maps.Keys(r.aliases))
	return r, nil
}

// Alias returns the alias with the given name.
func (r *Router) Alias(name string) (*Alias, bool) {
	a, ok := r.aliases[name]
	return a, ok
}

// Names returns every alias name, sorted.
func (r *Router) Names() []string {
	return slices.Clone(r.names)
}

// BreakerState is one target's circuit-breaker state, for metrics.
type BreakerState struct {
	Alias  string
	Target string // provider/model
	State  string // "closed", "open" or "half_open"
}

// BreakerStates returns every target's breaker state. A breaker reports
// open until a call after its cool-down moves it to half-open.
func (r *Router) BreakerStates() []BreakerState {
	var states []BreakerState
	for _, name := range r.names {
		for i := range r.aliases[name].Targets {
			t := &r.aliases[name].Targets[i]
			states = append(states, BreakerState{Alias: name, Target: t.String(), State: t.breaker.currentState().String()})
		}
	}
	return states
}

// Result says which target served a call, and how it got there.
type Result struct {
	Provider string // of the target that served the call, or the last one tried
	Model    string
	Attempts int // calls made, retries and fallbacks included
	// Tries is every attempt and every skipped target, in order: the
	// per-attempt story for the log.
	Tries []Try
}

// Try is one attempt at a target, or one target skipped.
type Try struct {
	Target     string  `json:"target"`  // provider/model
	Outcome    string  `json:"outcome"` // "ok", "skipped", or the error class
	Error      string  `json:"error,omitempty"`
	DurationMS float64 `json:"duration_ms"`
}

// InvalidRequestError means a request can't go to one of its alias's
// targets as it is. The router checks every target before the first
// attempt, so a fallback never turns a valid request into an upstream 400.
type InvalidRequestError struct {
	Alias   string
	Target  string
	Param   string
	Message string
}

func (e *InvalidRequestError) Error() string {
	return fmt.Sprintf("model %s can't take this request: %s: %s", e.Alias, e.Param, e.Message)
}

// ExhaustedError means every target of an alias failed or was skipped.
type ExhaustedError struct {
	Alias string
	// Last is the error from the last target tried, or nil if every target
	// was skipped by its breaker.
	Last error
	// CapacityOnly means no target failed outright: each one was skipped by
	// an open breaker or was rate limited. Retrying later can work, so the
	// client gets a 503 rather than a 502.
	CapacityOnly bool
	// RateLimited means at least one target was rate limited.
	RateLimited bool
	// RetryAfter, set only when CapacityOnly is, is the shortest wait any
	// target suggested: a Retry-After header, or an open breaker's
	// remaining cool-down.
	RetryAfter time.Duration
}

func (e *ExhaustedError) Error() string {
	if e.Last == nil {
		return fmt.Sprintf("model %s: every target's circuit breaker is open", e.Alias)
	}
	return fmt.Sprintf("model %s: every target failed; last: %v", e.Alias, e.Last)
}

func (e *ExhaustedError) Unwrap() error { return e.Last }

// validator is implemented by providers with rules of their own, such as
// Anthropic's narrower temperature range.
type validator interface {
	Validate(req *provider.ChatRequest) error
}

// validate checks req against every target of the alias.
func (r *Router) validate(alias *Alias, req *provider.ChatRequest) error {
	for i := range alias.Targets {
		t := &alias.Targets[i]
		if !t.Sampling && (req.Temperature != nil || req.TopP != nil) {
			param := "temperature"
			if req.Temperature == nil {
				param = "top_p"
			}
			return &InvalidRequestError{Alias: alias.Name, Target: t.String(), Param: param,
				Message: "one of its upstream models doesn't accept sampling parameters; leave temperature and top_p out"}
		}
		v, ok := t.Provider.(validator)
		if !ok {
			continue
		}
		if err := v.Validate(req); err != nil {
			param := ""
			if perr, ok := errors.AsType[*provider.ParamError](err); ok {
				param, err = perr.Param, errors.New(perr.Message)
			}
			return &InvalidRequestError{Alias: alias.Name, Target: t.String(), Param: param, Message: err.Error()}
		}
	}
	return nil
}

// Chat sends req to the alias's targets until one answers, within the
// total timeout. req is passed by value: each target gets its own copy with
// its own upstream model ID.
func (r *Router) Chat(ctx context.Context, alias *Alias, req provider.ChatRequest) (*provider.ChatResponse, Result, error) {
	var res Result
	if err := r.validate(alias, &req); err != nil {
		return nil, res, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, r.timeouts.Total, ErrTotalTimeout)
	defer cancel()

	var resp *provider.ChatResponse
	err := r.run(ctx, alias, &res, func(ctx context.Context, t *Target) (outcome, error) {
		treq := req
		treq.Model = t.Model
		var err error
		resp, err = t.Provider.Chat(ctx, &treq)
		err = timeoutCause(ctx, err)
		return breakerOutcome(err), err
	})
	return resp, res, err
}

// callFunc makes one attempt at a target. It returns what the attempt tells
// the breaker, and nil on success.
type callFunc func(ctx context.Context, t *Target) (outcome, error)

// run tries the alias's targets in order until one succeeds. A bad request
// or a cancellation stops it at once; anything else moves on to the next
// target once retries are used up. If every target fails or is skipped, it
// returns an *ExhaustedError.
func (r *Router) run(ctx context.Context, alias *Alias, res *Result, call callFunc) error {
	ex := &ExhaustedError{Alias: alias.Name, CapacityOnly: true}
	for i := range alias.Targets {
		t := &alias.Targets[i]
		err := r.tryTarget(ctx, t, res, call)
		if err == nil {
			return nil
		}
		switch class := provider.ClassOf(err); {
		case errors.Is(err, errBreakerOpen):
			ex.RetryAfter = shortest(ex.RetryAfter, t.breaker.retryIn())
			continue
		case class == provider.BadRequest, class == provider.Cancelled:
			return err
		case class == provider.RateLimited:
			ex.RateLimited = true
			ex.RetryAfter = shortest(ex.RetryAfter, retryAfter(err))
		default:
			ex.CapacityOnly = false
		}
		ex.Last = err
		if ctx.Err() != nil {
			break // the total deadline has passed: no time for another target
		}
	}
	if !ex.CapacityOnly {
		ex.RetryAfter = 0 // a hint about waiting is only useful when waiting can help
	}
	return ex
}

// tryTarget makes up to MaxAttemptsPerTarget attempts at one target. It
// retries an Unavailable target after a backoff, and a rate-limited one
// after its Retry-After if that is short. It returns nil on success, or the
// error that ended the attempts.
func (r *Router) tryTarget(ctx context.Context, t *Target, res *Result, call callFunc) error {
	for n := 1; ; n++ {
		p, ok := t.breaker.allow()
		if !ok {
			res.Tries = append(res.Tries, Try{Target: t.String(), Outcome: "skipped", Error: errBreakerOpen.Error()})
			return errBreakerOpen
		}
		res.Provider, res.Model = t.Provider.Name(), t.Model
		res.Attempts++

		start := time.Now()
		o, err := call(ctx, t)
		t.breaker.record(p, o)
		res.Tries = append(res.Tries, newTry(t, err, time.Since(start)))
		if err == nil {
			return nil
		}
		if n >= r.retry.MaxAttemptsPerTarget || ctx.Err() != nil {
			return err
		}

		var wait time.Duration
		switch provider.ClassOf(err) {
		case provider.Unavailable:
			wait = backoff(n, r.retry.BaseBackoff, r.retry.MaxBackoff, r.jitter)
		case provider.RateLimited:
			ra := retryAfter(err)
			if ra <= 0 || ra > r.retry.MaxRetryAfter {
				return err // no hint, or too long to wait: try the next target instead
			}
			wait = ra
		default:
			return err // a bad request, a config problem or a cancellation won't fix itself
		}
		if err := sleep(ctx, wait); err != nil {
			return timeoutCause(ctx, err)
		}
	}
}

func newTry(t *Target, err error, took time.Duration) Try {
	try := Try{Target: t.String(), Outcome: "ok", DurationMS: float64(took.Microseconds()) / 1000}
	if err != nil {
		try.Outcome = provider.ClassOf(err).String()
		try.Error = err.Error()
	}
	return try
}

// breakerOutcome is what an attempt's error tells the target's breaker.
// Only an Unavailable target counts against it; a rate limit, a bad
// request, a config problem or a cancellation says nothing about whether
// the target is up.
func breakerOutcome(err error) outcome {
	switch {
	case err == nil:
		return success
	case provider.ClassOf(err) == provider.Unavailable:
		return failure
	default:
		return neutral
	}
}

// retryAfter is the Retry-After an upstream error carried, if any.
func retryAfter(err error) time.Duration {
	if perr, ok := errors.AsType[*provider.Error](err); ok {
		return perr.RetryAfter
	}
	return 0
}

// shortest returns the smaller positive duration of a and b.
func shortest(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// sleep waits for d, or returns ctx's error if ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// timeoutCause returns the gateway timeout behind err, if there was one. A
// provider only sees its context end; the context's cause says why. Other
// errors, including a client's cancellation, are returned unchanged.
func timeoutCause(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err // a real upstream failure that happened to coincide
	}
	if cause := context.Cause(ctx); errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return err
}
