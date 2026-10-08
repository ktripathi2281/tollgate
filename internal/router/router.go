// Package router maps model aliases to upstream targets and calls them,
// within the gateway's timeouts.
//
// For now the router calls an alias's first target only. Retries, fallback
// to later targets and circuit breakers arrive in M3.
package router

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// Errors for calls ended by the gateway's own timeouts. Each one wraps
// context.DeadlineExceeded, so errors.Is(err, context.DeadlineExceeded)
// holds for all of them and provider.ClassOf treats them as Unavailable.
var (
	ErrFirstTokenTimeout = fmt.Errorf("no content within the first-token timeout: %w", context.DeadlineExceeded)
	ErrIdleTimeout       = fmt.Errorf("no chunk within the idle timeout: %w", context.DeadlineExceeded)
	ErrTotalTimeout      = fmt.Errorf("the request ran past its total timeout: %w", context.DeadlineExceeded)
)

// Target is one upstream provider and the model ID to request from it.
type Target struct {
	Provider provider.Provider
	Model    string
}

// Alias is a model name clients can request.
type Alias struct {
	Name             string
	DefaultMaxTokens int
	MaxTokensCeiling int
	Targets          []Target
}

// Router holds the aliases and the timeouts. It is read-only after New, so
// it is safe for concurrent use.
type Router struct {
	aliases  map[string]*Alias
	names    []string // sorted
	timeouts config.Timeouts
}

// New builds the aliases in models, using the providers built from the
// same config.
func New(models map[string]config.Model, providers map[string]provider.Provider, timeouts config.Timeouts) (*Router, error) {
	r := &Router{aliases: make(map[string]*Alias, len(models)), timeouts: timeouts}
	for name, m := range models {
		alias := &Alias{Name: name, DefaultMaxTokens: m.DefaultMaxTokens, MaxTokensCeiling: m.MaxTokensCeiling}
		for _, t := range m.Targets {
			p, ok := providers[t.Provider]
			if !ok {
				return nil, fmt.Errorf("alias %s: unknown provider %q", name, t.Provider)
			}
			alias.Targets = append(alias.Targets, Target{Provider: p, Model: t.Model})
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

// Result says which target served a call, for response headers and logs.
type Result struct {
	Provider string
	Model    string
	Attempts int
}

// Chat sends req to the alias's target and waits for the whole response,
// within the total timeout. req is passed by value: each target gets its
// own copy with its own upstream model ID.
func (r *Router) Chat(ctx context.Context, alias *Alias, req provider.ChatRequest) (*provider.ChatResponse, Result, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, r.timeouts.Total, ErrTotalTimeout)
	defer cancel()

	t := alias.Targets[0]
	req.Model = t.Model
	resp, err := t.Provider.Chat(ctx, &req)
	return resp, Result{Provider: t.Provider.Name(), Model: t.Model, Attempts: 1}, timeoutCause(ctx, err)
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
