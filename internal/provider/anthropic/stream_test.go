package anthropic

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/provider"
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

func drain(s provider.Stream) ([]*provider.Chunk, error) {
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
	s, err := adapter(t, u.srv.URL).ChatStream(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(s)
	if err != nil {
		t.Fatal(err)
	}

	if u.request.Header.Get("Accept") != "text/event-stream" {
		t.Error("a streaming request should accept text/event-stream")
	}

	// One chunk per event: 15 events in the fixture.
	if len(chunks) != 15 {
		t.Fatalf("got %d chunks, want 15", len(chunks))
	}
	var text strings.Builder
	reasoning, keepalives := 0, 0
	for _, c := range chunks[:14] {
		text.WriteString(c.Delta)
		if c.Reasoning {
			reasoning++
		}
		if c.IsKeepalive() {
			keepalives++
		}
	}
	if chunks[0].Model != "claude-x-2026" {
		t.Errorf("message_start chunk = %+v, want the model", chunks[0])
	}
	if text.String() != "Hello!" {
		t.Errorf("text = %q, want Hello!: thinking, citations and unknown events add nothing", text.String())
	}
	// The thinking block's start, its thinking delta and its signature delta.
	if reasoning != 3 {
		t.Errorf("got %d reasoning chunks, want 3", reasoning)
	}
	// Everything before message_stop but the two text deltas carries
	// nothing for the client.
	if keepalives != 12 {
		t.Errorf("got %d keepalives, want 12", keepalives)
	}

	// message_stop gives the finish and the usage: 20 input plus 5 cached,
	// and the last cumulative output count, 15.
	last := chunks[14]
	if last.FinishReason != provider.FinishStop || last.Usage == nil ||
		*last.Usage != (provider.Usage{PromptTokens: 25, CompletionTokens: 15}) {
		t.Errorf("final chunk = %+v, usage %+v", last, last.Usage)
	}
}

func TestStreamFailures(t *testing.T) {
	tests := []struct {
		fixture     string
		wantMessage string
	}{
		// An error event after content has started.
		{"stream_error.txt", "overloaded_error: Overloaded"},
		// The stream ends without message_stop: never a silent truncation.
		{"stream_truncated.txt", "request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			u := streamUpstream(t, fixture(t, tt.fixture))
			s, err := adapter(t, u.srv.URL).ChatStream(t.Context(), request())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			chunks, err := drain(s)
			var text strings.Builder
			for _, c := range chunks {
				text.WriteString(c.Delta)
			}
			if text.Len() == 0 {
				t.Error("no text arrived before the failure")
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

func TestStreamRateLimitEvent(t *testing.T) {
	u := streamUpstream(t, []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n"))
	s, err := adapter(t, u.srv.URL).ChatStream(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Next(); provider.ClassOf(err) != provider.RateLimited {
		t.Errorf("got %v, want RateLimited", err)
	}
}

func TestStreamErrorStatus(t *testing.T) {
	u := newUpstream(t, 529, nil, fixture(t, "error_529.json"))
	if _, err := adapter(t, u.srv.URL).ChatStream(t.Context(), request()); provider.ClassOf(err) != provider.Unavailable {
		t.Errorf("got %v, want Unavailable", err)
	}
}
