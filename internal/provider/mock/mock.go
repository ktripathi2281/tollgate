// Package mock is a fake LLM provider for tests, benchmarks and the local
// demo. It generates text locally, with configurable latency and failures,
// so nothing needs a real provider or an API key.
//
// It comes in two forms that share one implementation: Provider, used
// in-process, and Handler, an OpenAI-compatible HTTP API around a Provider,
// served by cmd/mockupstream.
package mock

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// Config controls the mock's behaviour.
type Config struct {
	// TTFT is the time to the first token.
	TTFT time.Duration
	// TokenInterval is the time between tokens.
	TokenInterval time.Duration
	// OutputTokens is how many tokens a reply has, or fewer if the request's
	// MaxTokens is lower.
	OutputTokens int
	// ErrorRate is the chance, from 0 to 1, that a call fails with a 500.
	ErrorRate float64
	// Status, if non-zero, makes every call fail with this HTTP status.
	Status int
	// Hang makes every call block until its context is done.
	Hang bool
	// Seed makes the generated text and the injected failures repeatable.
	Seed uint64
}

// Provider is the in-process mock. It is safe for concurrent use.
type Provider struct {
	name string
	cfg  Config

	mu  sync.Mutex
	rng *rand.Rand // guarded by mu: a rand.Rand is not safe for concurrent use

	// OnCall, if set, is called at the start of every call. OnCancel, if
	// set, is called when a call stops because its context is done. Tests
	// use them to see a request arrive and to see a cancellation reach the
	// provider. Set them before the first call.
	OnCall   func()
	OnCancel func()
}

var _ provider.Provider = (*Provider)(nil)

// New returns a mock provider named name.
func New(name string, cfg Config) *Provider {
	return &Provider{
		name: name,
		cfg:  cfg,
		rng:  rand.New(rand.NewPCG(cfg.Seed, cfg.Seed)),
	}
}

// Name returns the provider's name.
func (p *Provider) Name() string { return p.name }

// Chat simulates a complete, non-streaming call: it waits for the time the
// reply would take to generate, then returns it.
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	if p.OnCall != nil {
		p.OnCall()
	}
	if p.cfg.Hang {
		<-ctx.Done()
		return nil, p.stopped(ctx)
	}
	if err := p.injectedError(); err != nil {
		return nil, err
	}

	n, finish := p.cfg.OutputTokens, provider.FinishStop
	if req.MaxTokens > 0 && req.MaxTokens < n {
		n, finish = req.MaxTokens, provider.FinishLength
	}

	generation := p.cfg.TTFT + time.Duration(n)*p.cfg.TokenInterval
	if err := sleep(ctx, generation); err != nil {
		return nil, p.stopped(ctx)
	}

	return &provider.ChatResponse{
		Model:        req.Model,
		Content:      p.text(n),
		FinishReason: finish,
		Usage: provider.Usage{
			PromptTokens:     promptTokens(req.Messages),
			CompletionTokens: n,
		},
	}, nil
}

// stopped reports that a call ended because ctx is done.
func (p *Provider) stopped(ctx context.Context) error {
	if p.OnCancel != nil {
		p.OnCancel()
	}
	return fmt.Errorf("mock %s: %w", p.name, ctx.Err())
}

// injectedError returns the configured failure for this call, if any.
func (p *Provider) injectedError() error {
	status := p.cfg.Status
	if status == 0 && p.cfg.ErrorRate > 0 {
		p.mu.Lock()
		fail := p.rng.Float64() < p.cfg.ErrorRate
		p.mu.Unlock()
		if fail {
			status = 500
		}
	}
	if status == 0 {
		return nil
	}
	err := &provider.Error{
		Provider: p.name,
		Class:    provider.ClassifyStatus(status),
		Status:   status,
		Message:  fmt.Sprintf("mock failure with status %d", status),
	}
	if status == 429 {
		err.RetryAfter = time.Second
	}
	return err
}

// words are what the mock's replies are made of, one word per token.
var words = []string{
	"the", "gateway", "routes", "each", "request", "to", "a", "model",
	"and", "records", "what", "it", "cost", "in", "micro", "dollars",
}

// text returns n pseudo-random words.
func (p *Provider) text(n int) string {
	out := make([]string, n)
	p.mu.Lock()
	for i := range out {
		out[i] = words[p.rng.IntN(len(words))]
	}
	p.mu.Unlock()
	return strings.Join(out, " ")
}

// promptTokens estimates prompt tokens at about four characters per token,
// as a real provider would count them.
func promptTokens(messages []provider.Message) int {
	chars := 0
	for _, m := range messages {
		for _, part := range m.Parts {
			chars += utf8.RuneCountInString(part)
		}
	}
	return max(1, (chars+3)/4)
}

// sleep waits for d, or returns ctx's error if ctx is done first.
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
