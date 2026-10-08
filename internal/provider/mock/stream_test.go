package mock

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// arrival is a chunk and when it arrived, relative to the stream's start.
type arrival struct {
	chunk *provider.Chunk
	at    time.Duration
}

// drain reads s until it ends and returns what arrived and the final error
// (nil for a normal end).
func drain(s provider.Stream, start time.Time) ([]arrival, error) {
	var got []arrival
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, arrival{c, time.Since(start)})
	}
}

func TestStreamTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New("mock", Config{TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond, OutputTokens: 3})
		start := time.Now()
		s, err := p.ChatStream(t.Context(), testRequest(10))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		got, err := drain(s, start)
		if err != nil {
			t.Fatalf("stream failed: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d chunks, want 3 content chunks and a final one", len(got))
		}
		// Fake time makes the arrival times exact.
		wantAt := []time.Duration{100 * time.Millisecond, 110 * time.Millisecond, 120 * time.Millisecond, 120 * time.Millisecond}
		var text strings.Builder
		for i, a := range got {
			if a.at != wantAt[i] {
				t.Errorf("chunk %d arrived at %v, want %v", i, a.at, wantAt[i])
			}
			text.WriteString(a.chunk.Delta)
		}
		if words := strings.Fields(text.String()); len(words) != 3 {
			t.Errorf("text %q has %d words, want 3", text.String(), len(words))
		}
		if got[0].chunk.Model != "mock-1" {
			t.Errorf("first chunk model = %q, want mock-1", got[0].chunk.Model)
		}
		last := got[3].chunk
		wantUsage := provider.Usage{PromptTokens: 6, CompletionTokens: 3}
		if last.FinishReason != provider.FinishStop || last.Usage == nil || *last.Usage != wantUsage || last.Delta != "" {
			t.Errorf("final chunk = %+v, want finish stop and usage %+v", last, wantUsage)
		}
	})
}

func TestStreamCutShortByMaxTokens(t *testing.T) {
	s, err := New("mock", Config{OutputTokens: 10}).ChatStream(t.Context(), testRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	got, err := drain(s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	last := got[len(got)-1].chunk
	if len(got) != 3 || last.FinishReason != provider.FinishLength || last.Usage.CompletionTokens != 2 {
		t.Errorf("got %d chunks ending with %+v; want 2 content chunks, then finish length with 2 tokens", len(got), last)
	}
}

func TestStreamFailAtChunk(t *testing.T) {
	tests := []struct {
		name         string
		failAt       int
		wantContent  int
		wantFailedAt time.Duration
	}{
		{"error event before any content", 1, 0, 100 * time.Millisecond},
		{"error event mid-stream", 3, 2, 120 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := New("mock", Config{TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond, OutputTokens: 5, FailAtChunk: tt.failAt})
				start := time.Now()
				s, err := p.ChatStream(t.Context(), testRequest(10))
				if err != nil {
					t.Fatal(err)
				}
				got, err := drain(s, start)
				if provider.ClassOf(err) != provider.Unavailable {
					t.Errorf("got error %v, want an Unavailable error", err)
				}
				if len(got) != tt.wantContent {
					t.Errorf("got %d chunks before the error, want %d", len(got), tt.wantContent)
				}
				if failedAt := time.Since(start); failedAt != tt.wantFailedAt {
					t.Errorf("failed at %v, want %v", failedAt, tt.wantFailedAt)
				}
				if _, err := s.Next(); !errors.Is(err, io.EOF) {
					t.Errorf("Next after the error = %v, want io.EOF", err)
				}
			})
		})
	}
}

func TestStreamStallsUntilCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := false
		p := New("mock", Config{TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond, OutputTokens: 5, StallAtChunk: 2})
		p.OnCancel = func() { cancelled = true }

		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		start := time.Now()
		s, err := p.ChatStream(ctx, testRequest(10))
		if err != nil {
			t.Fatal(err)
		}
		got, err := drain(s, start)
		if len(got) != 1 {
			t.Errorf("got %d chunks before the stall, want 1", len(got))
		}
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Hour {
			t.Errorf("stream ended with %v after %v, want the context's deadline after an hour", err, time.Since(start))
		}
		if !cancelled {
			t.Error("OnCancel was not called")
		}
	})
}

func TestStreamOpenFailures(t *testing.T) {
	t.Run("forced status", func(t *testing.T) {
		_, err := New("mock", Config{OutputTokens: 1, Status: 503}).ChatStream(t.Context(), testRequest(1))
		if perr, ok := errors.AsType[*provider.Error](err); !ok || perr.Status != 503 {
			t.Errorf("got %v, want a provider error with status 503", err)
		}
	})
	t.Run("hang before the stream opens", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if _, err := New("mock", Config{OutputTokens: 1, Hang: true}).ChatStream(ctx, testRequest(1)); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("got %v, want the context's deadline", err)
			}
		})
	})
}

func TestStreamClose(t *testing.T) {
	s, err := New("mock", Config{OutputTokens: 5}).ChatStream(t.Context(), testRequest(5))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next after Close = %v, want io.EOF", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}
