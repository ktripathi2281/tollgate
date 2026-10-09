package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

// streamUpstream serves an SSE fixture one event at a time, flushing each.
func streamUpstream(t *testing.T, body []byte) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.request = r
		w.Header().Set("Content-Type", "text/event-stream")
		for event := range bytes.SplitAfterSeq(body, []byte("\n\n")) {
			_, _ = w.Write(event)
			http.NewResponseController(w).Flush()
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// drain reads a stream to its end.
func drain(t *testing.T, s provider.Stream) ([]*provider.Chunk, error) {
	t.Helper()
	var chunks []*provider.Chunk
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
}

func TestStream(t *testing.T) {
	u := streamUpstream(t, fixture(t, "stream.txt"))
	s, err := adapter(t, u.srv.URL, "").ChatStream(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	// role, "Hello", " world", finish, usage; then [DONE] ends it
	if len(chunks) != 5 {
		t.Fatalf("got %d chunks, want 5: %+v", len(chunks), chunks)
	}
	if first := chunks[0]; !first.IsKeepalive() || first.Model != "gpt-x-2026-09-01" {
		t.Errorf("first chunk = %+v, want a keepalive carrying the model", first)
	}
	if chunks[1].Delta != "Hello" || chunks[2].Delta != " world" {
		t.Errorf("deltas = %q, %q", chunks[1].Delta, chunks[2].Delta)
	}
	if chunks[3].FinishReason != provider.FinishLength {
		t.Errorf("finish = %q, want length", chunks[3].FinishReason)
	}
	if u := chunks[4].Usage; u == nil || *u != (provider.Usage{PromptTokens: 12, CompletionTokens: 2}) {
		t.Errorf("usage chunk = %+v", chunks[4])
	}
}

func TestStreamAsksForUsage(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	s, err := adapter(t, srv.URL, "").ChatStream(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _ = drain(t, s)
	for _, want := range []string{`"stream":true`, `"stream_options":{"include_usage":true}`} {
		if !strings.Contains(body, want) {
			t.Errorf("request body %s lacks %s", body, want)
		}
	}
}

func TestStreamFailures(t *testing.T) {
	tests := []struct {
		fixture     string
		wantText    string
		wantMessage string
	}{
		// An error object in place of a chunk, after some content.
		{"stream_error.txt", "Partial", "The server had an error"},
		// The connection ends without [DONE]: never a silent truncation.
		{"stream_truncated.txt", "Cut", "request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			u := streamUpstream(t, fixture(t, tt.fixture))
			s, err := adapter(t, u.srv.URL, "").ChatStream(t.Context(), request())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			chunks, err := drain(t, s)
			var text strings.Builder
			for _, c := range chunks {
				text.WriteString(c.Delta)
			}
			if text.String() != tt.wantText {
				t.Errorf("text before the failure = %q, want %q", text.String(), tt.wantText)
			}
			perr, ok := errors.AsType[*provider.Error](err)
			if !ok || perr.Class != provider.Unavailable || !strings.Contains(perr.Error(), tt.wantMessage) {
				t.Errorf("got %v, want an Unavailable error mentioning %q", err, tt.wantMessage)
			}
			if _, err := s.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("Next after the failure = %v, want io.EOF", err)
			}
		})
	}
}

func TestStreamErrorStatus(t *testing.T) {
	u := newUpstream(t, 429, http.Header{"Retry-After": {"2"}}, fixture(t, "error_429_rate.json"))
	_, err := adapter(t, u.srv.URL, "").ChatStream(t.Context(), request())
	if perr, ok := errors.AsType[*provider.Error](err); !ok || perr.Class != provider.RateLimited || perr.RetryAfter != 2*time.Second {
		t.Errorf("got %v, want a rate limit with Retry-After 2s", err)
	}
}

func TestStreamCancel(t *testing.T) {
	seen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"model":"m","choices":[{"delta":{"content":"a"}}]}`+"\n\n")
		http.NewResponseController(w).Flush()
		<-r.Context().Done() // stall until the gateway gives up
		close(seen)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	s, err := adapter(t, srv.URL, "").ChatStream(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c, err := s.Next(); err != nil || c.Delta != "a" {
		t.Fatalf("first chunk = %+v, %v", c, err)
	}
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, err := s.Next(); provider.ClassOf(err) != provider.Cancelled {
		t.Errorf("got %v (%v), want Cancelled", err, provider.ClassOf(err))
	}
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Error("the upstream never saw the cancellation")
	}
}

// The adapter against the mock's OpenAI-compatible server: the same HTTP
// path that cmd/mockupstream serves for benchmarks.
func TestAgainstMockUpstream(t *testing.T) {
	srv := httptest.NewServer(mock.Handler(mock.New("mock", mock.Config{OutputTokens: 4, Seed: 1})))
	defer srv.Close()
	p := adapter(t, srv.URL, "")

	resp, err := p.Chat(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(resp.Content)) != 4 || resp.Usage == nil || resp.Usage.CompletionTokens != 4 {
		t.Errorf("Chat = %+v", resp)
	}

	s, err := p.ChatStream(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var finish provider.FinishReason
	var usage *provider.Usage
	for _, c := range chunks {
		text.WriteString(c.Delta)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if len(strings.Fields(text.String())) != 4 || finish != provider.FinishStop || usage == nil || usage.CompletionTokens != 4 {
		t.Errorf("stream: text %q, finish %q, usage %+v", text.String(), finish, usage)
	}
}
