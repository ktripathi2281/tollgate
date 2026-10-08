package router

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// step is one thing a scripted stream does: wait, then return a chunk or an
// error.
type step struct {
	after time.Duration
	chunk *provider.Chunk
	err   error
}

func content(after time.Duration, text string) step {
	return step{after: after, chunk: &provider.Chunk{Delta: text}}
}

func keepalive(after time.Duration) step {
	return step{after: after, chunk: &provider.Chunk{}}
}

// forever is longer than any test runs: the stream stalls.
const forever = 1000 * time.Hour

// scripted is a provider whose streams play back a fixed script.
type scripted struct {
	steps   []step
	openErr error
	opened  context.Context // the context of the last stream opened
}

func (p *scripted) Name() string { return "scripted" }

func (p *scripted) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("scripted: Chat is not used")
}

func (p *scripted) ChatStream(ctx context.Context, _ *provider.ChatRequest) (provider.Stream, error) {
	p.opened = ctx
	if p.openErr != nil {
		return nil, p.openErr
	}
	return &scriptStream{ctx: ctx, steps: p.steps}, nil
}

type scriptStream struct {
	ctx    context.Context
	steps  []step
	closed bool
}

func (s *scriptStream) Next() (*provider.Chunk, error) {
	if s.closed || len(s.steps) == 0 {
		return nil, io.EOF
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	timer := time.NewTimer(st.after)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.ctx.Done():
		s.closed = true
		return nil, s.ctx.Err()
	}
	if st.err != nil {
		s.closed = true
		return nil, st.err
	}
	return st.chunk, nil
}

func (s *scriptStream) Close() error {
	s.closed = true
	return nil
}

var testTimeouts = config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: time.Minute}

// streamRouter returns a router with one alias, "a", backed by p.
func streamRouter(t *testing.T, p provider.Provider, timeouts config.Timeouts) (*Router, *Alias) {
	t.Helper()
	r, err := New(map[string]config.Model{
		"a": {DefaultMaxTokens: 10, MaxTokensCeiling: 10, Targets: []config.Target{{Provider: p.Name(), Model: "m-1"}}},
	}, map[string]provider.Provider{p.Name(): p}, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := r.Alias("a")
	return r, alias
}

// collect reads s to the end and returns the text it carried and the error
// that ended it (nil for a normal end).
func collect(s *Stream) (string, error) {
	var text strings.Builder
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return text.String(), nil
		}
		if err != nil {
			return text.String(), err
		}
		text.WriteString(c.Delta)
	}
}

func TestStreamCommitsOnFirstContent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		usage := &provider.Usage{PromptTokens: 2, CompletionTokens: 2}
		p := &scripted{steps: []step{
			keepalive(10 * time.Millisecond),
			{after: 10 * time.Millisecond, chunk: &provider.Chunk{Model: "m-1-2026"}},
			content(10*time.Millisecond, "Hi"),
			content(10*time.Millisecond, " there"),
			{chunk: &provider.Chunk{FinishReason: provider.FinishStop, Usage: usage}},
		}}
		r, alias := streamRouter(t, p, testTimeouts)

		start := time.Now()
		s, res, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if committed := time.Since(start); committed != 30*time.Millisecond {
			t.Errorf("committed after %v, want 30ms, at the first content", committed)
		}
		if res != (Result{Provider: "scripted", Model: "m-1", Attempts: 1}) {
			t.Errorf("Result = %+v", res)
		}
		if s.Model() != "m-1-2026" {
			t.Errorf("Model() = %q, want the model the upstream reported", s.Model())
		}

		var got []*provider.Chunk
		for {
			c, err := s.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, c)
		}
		if len(got) != 3 || got[0].Delta != "Hi" || got[1].Delta != " there" || got[2].Usage != usage {
			t.Errorf("chunks = %+v, want Hi, there, then the finish with usage", got)
		}
	})
}

func TestStreamFirstTokenTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Keepalives arrive, but no content: they must not hold the
		// first-token timeout off.
		p := &scripted{steps: []step{
			keepalive(300 * time.Millisecond), keepalive(300 * time.Millisecond),
			keepalive(300 * time.Millisecond), keepalive(300 * time.Millisecond),
			content(0, "late"),
		}}
		r, alias := streamRouter(t, p, testTimeouts)

		start := time.Now()
		_, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if !errors.Is(err, ErrFirstTokenTimeout) || time.Since(start) != time.Second {
			t.Errorf("got %v after %v, want ErrFirstTokenTimeout after 1s", err, time.Since(start))
		}
		if !errors.Is(err, context.DeadlineExceeded) || provider.ClassOf(err) != provider.Unavailable {
			t.Errorf("a timeout must be a deadline and Unavailable, got %v (%v)", err, provider.ClassOf(err))
		}
	})
}

func TestStreamFirstTokenTimeoutWhileOpening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The upstream never even answers: ChatStream itself blocks.
		r, alias := streamRouter(t, hangingOpen{}, testTimeouts)
		start := time.Now()
		_, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if !errors.Is(err, ErrFirstTokenTimeout) || time.Since(start) != time.Second {
			t.Errorf("got %v after %v, want ErrFirstTokenTimeout after 1s", err, time.Since(start))
		}
	})
}

// hangingOpen is a provider whose ChatStream blocks until its context ends.
type hangingOpen struct{}

func (hangingOpen) Name() string { return "hanging" }
func (hangingOpen) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (hangingOpen) ChatStream(ctx context.Context, _ *provider.ChatRequest) (provider.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStreamIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &scripted{steps: []step{content(10*time.Millisecond, "a"), content(forever, "b")}}
		r, alias := streamRouter(t, p, testTimeouts)

		start := time.Now()
		s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		text, err := collect(s)
		if text != "a" || !errors.Is(err, ErrIdleTimeout) {
			t.Errorf("got %q then %v, want a then ErrIdleTimeout", text, err)
		}
		if elapsed := time.Since(start); elapsed != 10*time.Millisecond+time.Second {
			t.Errorf("timed out after %v, want 1.01s: the idle timeout counts from the last chunk", elapsed)
		}
	})
}

func TestStreamKeepalivesResetIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &scripted{steps: []step{
			content(0, "a"),
			keepalive(800 * time.Millisecond), keepalive(800 * time.Millisecond), keepalive(800 * time.Millisecond),
			content(800*time.Millisecond, "b"),
		}}
		r, alias := streamRouter(t, p, testTimeouts)

		s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// 3.2s pass between "a" and "b", longer than the idle timeout, but
		// no single gap is.
		if text, err := collect(s); text != "ab" || err != nil {
			t.Errorf("got %q, %v; want ab and a normal end", text, err)
		}
	})
}

func TestStreamTotalTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Chunks at 0.6s, 1.2s, 1.8s and 2.4s: the deadline at 2s falls
		// between two of them.
		var steps []step
		for range 10 {
			steps = append(steps, content(600*time.Millisecond, "x"))
		}
		timeouts := config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: 2 * time.Second}
		r, alias := streamRouter(t, &scripted{steps: steps}, timeouts)

		start := time.Now()
		s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		text, err := collect(s)
		if !errors.Is(err, ErrTotalTimeout) || time.Since(start) != 2*time.Second || text != "xxx" {
			t.Errorf("got %q then %v after %v; want xxx then ErrTotalTimeout after 2s", text, err, time.Since(start))
		}
	})
}

func TestStreamFailureBeforeCommit(t *testing.T) {
	upstream := &provider.Error{Provider: "scripted", Class: provider.Unavailable, Message: "overloaded"}
	tests := []struct {
		name string
		p    *scripted
	}{
		{"opening fails", &scripted{openErr: upstream}},
		{"error event before any content", &scripted{steps: []step{keepalive(0), {err: upstream}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, alias := streamRouter(t, tt.p, testTimeouts)
			_, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
			if !errors.Is(err, upstream) {
				t.Errorf("got %v, want the upstream error", err)
			}
		})
	}
}

func TestStreamEmptyStreamCommits(t *testing.T) {
	r, alias := streamRouter(t, &scripted{}, testTimeouts)
	s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next = %v, want io.EOF", err)
	}
}

func TestStreamKeepsUsageThatArrivesBeforeContent(t *testing.T) {
	usage := &provider.Usage{PromptTokens: 5}
	p := &scripted{steps: []step{{chunk: &provider.Chunk{Usage: usage}}, content(0, "a")}}
	r, alias := streamRouter(t, p, testTimeouts)
	s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, _ := s.Next()
	second, _ := s.Next()
	if first == nil || first.Usage != usage || second == nil || second.Delta != "a" {
		t.Errorf("got %+v then %+v, want the usage chunk then the content", first, second)
	}
}

func TestStreamCloseCancelsUpstream(t *testing.T) {
	p := &scripted{steps: []step{content(0, "a"), content(forever, "b")}}
	r, alias := streamRouter(t, p, testTimeouts)
	s, _, err := r.ChatStream(t.Context(), alias, provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p.opened.Err() != nil {
		t.Fatal("the upstream context ended before Close")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if p.opened.Err() == nil {
		t.Error("Close did not cancel the upstream call")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestStreamClientCancelIsNotATimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &scripted{steps: []step{content(0, "a"), content(forever, "b")}}
		r, alias := streamRouter(t, p, testTimeouts)
		ctx, cancel := context.WithCancel(t.Context())
		s, _, err := r.ChatStream(ctx, alias, provider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(100*time.Millisecond, cancel)
		if _, err := s.Next(); provider.ClassOf(err) != provider.Cancelled {
			t.Errorf("got %v (%v), want Cancelled", err, provider.ClassOf(err))
		}
	})
}
