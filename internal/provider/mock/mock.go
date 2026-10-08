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
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/usage"
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
	// Hang makes every call block until its context is done, before any
	// response: for a stream, before ChatStream returns.
	Hang bool
	// Seed makes the generated text and the injected failures repeatable.
	Seed uint64
	// FailAtChunk, if non-zero, makes a stream fail where content chunk
	// number FailAtChunk (counting from 1) would be, like an error event
	// arriving mid-stream. 1 fails before any content.
	FailAtChunk int
	// StallAtChunk, if non-zero, makes a stream stop sending where content
	// chunk number StallAtChunk would be, until its context is done.
	StallAtChunk int
}

// Provider is the in-process mock. It is safe for concurrent use.
type Provider struct {
	name string
	cfg  Config

	mu  sync.Mutex
	rng *rand.Rand // guarded by mu: a rand.Rand is not safe for concurrent use

	// OnCall, if set, is called at the start of every call. OnCancel, if
	// set, is called when a call or stream stops because its context is
	// done. Tests use them to see a request arrive and to see a
	// cancellation reach the provider. Set them before the first call.
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
	if err := p.start(ctx); err != nil {
		return nil, err
	}
	n, finish := p.replyLength(req)

	generation := p.cfg.TTFT + time.Duration(n)*p.cfg.TokenInterval
	if err := sleep(ctx, generation); err != nil {
		return nil, p.stopped(ctx)
	}

	return &provider.ChatResponse{
		Model:        req.Model,
		Content:      p.text(n),
		FinishReason: finish,
		Usage: provider.Usage{
			PromptTokens:     usage.EstimatePrompt(req.Messages),
			CompletionTokens: n,
		},
	}, nil
}

// ChatStream simulates a streaming call. The first content chunk arrives
// after TTFT and each later one after TokenInterval, one word per chunk. A
// final chunk carries the finish reason and the usage.
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.Stream, error) {
	if err := p.start(ctx); err != nil {
		return nil, err
	}
	n, finish := p.replyLength(req)
	return &stream{
		p:      p,
		ctx:    ctx,
		model:  req.Model,
		n:      n,
		finish: finish,
		prompt: usage.EstimatePrompt(req.Messages),
	}, nil
}

// start runs the steps every call begins with: the OnCall hook, then a hang
// or an injected failure if the config asks for one.
func (p *Provider) start(ctx context.Context) error {
	if p.OnCall != nil {
		p.OnCall()
	}
	if p.cfg.Hang {
		<-ctx.Done()
		return p.stopped(ctx)
	}
	return p.injectedError()
}

// replyLength returns how many tokens to generate for req and why the reply
// ends: naturally, or cut short by the request's MaxTokens.
func (p *Provider) replyLength(req *provider.ChatRequest) (int, provider.FinishReason) {
	if req.MaxTokens > 0 && req.MaxTokens < p.cfg.OutputTokens {
		return req.MaxTokens, provider.FinishLength
	}
	return p.cfg.OutputTokens, provider.FinishStop
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

// stream is the mock's provider.Stream.
type stream struct {
	p      *Provider
	ctx    context.Context // the context ChatStream was called with
	model  string
	n      int // content chunks to send
	finish provider.FinishReason
	prompt int // estimated prompt tokens

	sent int  // content chunks sent so far
	done bool // the stream has ended, normally or not
}

// Next returns the next chunk: content chunks one word at a time, then a
// final chunk with the finish reason and usage, then io.EOF.
func (s *stream) Next() (*provider.Chunk, error) {
	if s.done {
		return nil, io.EOF
	}
	if s.sent == s.n {
		s.done = true
		return &provider.Chunk{
			FinishReason: s.finish,
			Usage:        &provider.Usage{PromptTokens: s.prompt, CompletionTokens: s.n},
		}, nil
	}

	next := s.sent + 1
	cfg := s.p.cfg
	if next == cfg.StallAtChunk {
		<-s.ctx.Done()
		s.done = true
		return nil, s.p.stopped(s.ctx)
	}
	wait := cfg.TokenInterval
	if next == 1 {
		wait = cfg.TTFT
	}
	if err := sleep(s.ctx, wait); err != nil {
		s.done = true
		return nil, s.p.stopped(s.ctx)
	}
	if next == cfg.FailAtChunk {
		s.done = true
		return nil, &provider.Error{
			Provider: s.p.name,
			Class:    provider.Unavailable,
			Message:  fmt.Sprintf("mock error event at chunk %d", next),
		}
	}

	s.sent = next
	chunk := &provider.Chunk{Delta: s.p.text(1)}
	if next == 1 {
		chunk.Model = s.model
	} else {
		chunk.Delta = " " + chunk.Delta
	}
	return chunk, nil
}

// Close ends the stream. The mock holds no resources, so it only makes
// further calls to Next return io.EOF.
func (s *stream) Close() error {
	s.done = true
	return nil
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
