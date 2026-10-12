package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

// scripted is a provider whose calls run the given functions.
type scripted struct {
	chat   func(ctx context.Context) (*provider.ChatResponse, error)
	stream func(ctx context.Context) (provider.Stream, error)
}

func (p *scripted) Name() string { return "scripted" }

func (p *scripted) Chat(ctx context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	return p.chat(ctx)
}

func (p *scripted) ChatStream(ctx context.Context, _ *provider.ChatRequest) (provider.Stream, error) {
	return p.stream(ctx)
}

// leavesAfterFirstChunk is a stream that sends one chunk, then has the
// client go away and waits for the cancellation to arrive.
type leavesAfterFirstChunk struct {
	ctx    context.Context
	leave  context.CancelFunc
	chunks int
}

func (s *leavesAfterFirstChunk) Next() (*provider.Chunk, error) {
	s.chunks++
	if s.chunks == 1 {
		return &provider.Chunk{Delta: "Hello there, this is the first chunk."}, nil
	}
	s.leave()
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (s *leavesAfterFirstChunk) Close() error { return nil }

// Acceptance (M4): token reservations are refunded on success, failure and
// cancellation. Each case checks that the one reservation got exactly one
// refund, and how much of it was counted as used.
func TestTokenReservationsAreRefunded(t *testing.T) {
	const stream = `{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`
	tests := []struct {
		name string
		body string
		// provider builds the case's provider. leave cancels the request,
		// as a client that goes away would.
		provider func(leave context.CancelFunc) provider.Provider
		wantUsed bool // whether some tokens should count as used
	}{
		{
			name:     "success",
			body:     chatBody,
			provider: func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{OutputTokens: 3}) },
			wantUsed: true,
		},
		{
			name:     "stream success",
			body:     stream,
			provider: func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{OutputTokens: 3}) },
			wantUsed: true,
		},
		{
			name:     "upstream failure",
			body:     chatBody,
			provider: func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{Status: 503}) },
		},
		{
			name:     "stream failure before the commit point",
			body:     stream,
			provider: func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{Status: 503}) },
		},
		{
			name: "stream failure after the commit point",
			body: stream,
			provider: func(context.CancelFunc) provider.Provider {
				return mock.New("mock", mock.Config{OutputTokens: 5, FailAtChunk: 3})
			},
			wantUsed: true, // the prompt and two chunks, estimated
		},
		{
			name: "client gone during the call",
			body: chatBody,
			provider: func(leave context.CancelFunc) provider.Provider {
				p := mock.New("mock", mock.Config{Hang: true})
				p.OnCall = leave
				return p
			},
		},
		{
			name: "client gone mid-stream",
			body: stream,
			provider: func(leave context.CancelFunc) provider.Provider {
				return &scripted{stream: func(ctx context.Context) (provider.Stream, error) {
					return &leavesAfterFirstChunk{ctx: ctx, leave: leave}, nil
				}}
			},
			wantUsed: true,
		},
		{
			name: "panic",
			body: chatBody,
			provider: func(context.CancelFunc) provider.Provider {
				return &scripted{chat: func(context.Context) (*provider.ChatResponse, error) { panic("provider bug") }}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, leave := context.WithCancel(t.Context())
			defer leave()
			s := testServer(t, tt.provider(leave), nil)
			lim := s.limiter.(*fakeLimiter)

			do(ctx, s, http.MethodPost, "/v1/chat/completions", tt.body)

			reserved, refunds := lim.snapshot()
			if len(reserved) != 1 || len(refunds) != 1 {
				t.Fatalf("%d reservations and %d refunds, want one of each", len(reserved), len(refunds))
			}
			if refunds[0].reserved != reserved[0] {
				t.Errorf("refund settles %d tokens, but %d were reserved", refunds[0].reserved, reserved[0])
			}
			if used := refunds[0].used; (used > 0) != tt.wantUsed {
				t.Errorf("used = %d; want some used: %v", used, tt.wantUsed)
			}
		})
	}
}

// Requests turned away before the reservation reserve nothing and refund
// nothing.
func TestRejectedRequestsReserveNothing(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	lim := s.limiter.(*fakeLimiter)
	for _, body := range []string{
		`{"model": "no-such-alias", "messages": [{"role": "user", "content": "hi"}]}`,
		`{"model": "mock-fast", "max_tokens": 1000, "messages": [{"role": "user", "content": "hi"}]}`,
		`{"model": "mock-fast", "messages": "not a list"}`,
	} {
		if rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", body); rec.Code < 400 {
			t.Fatalf("%s: status %d, want an error", body, rec.Code)
		}
	}
	if rec := doAs(t.Context(), s, "", http.MethodPost, "/v1/chat/completions", chatBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", rec.Code)
	}
	if reserved, refunds := lim.snapshot(); len(reserved) != 0 || len(refunds) != 0 {
		t.Errorf("reservations %v, refunds %v; want none", reserved, refunds)
	}
}
