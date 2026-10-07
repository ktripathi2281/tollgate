package mock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func testRequest(maxTokens int) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:     "mock-1",
		Messages:  []provider.Message{{Role: provider.RoleUser, Parts: []string{"Say hello to the gateway"}}}, // 24 characters
		MaxTokens: maxTokens,
	}
}

func TestChatReply(t *testing.T) {
	tests := []struct {
		name       string
		maxTokens  int
		wantTokens int
		wantFinish provider.FinishReason
	}{
		{"full reply", 100, 5, provider.FinishStop},
		{"exactly the limit", 5, 5, provider.FinishStop},
		{"cut short by max tokens", 3, 3, provider.FinishLength},
		{"no limit", 0, 5, provider.FinishStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New("mock", Config{OutputTokens: 5, Seed: 1})
			resp, err := p.Chat(t.Context(), testRequest(tt.maxTokens))
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := len(strings.Fields(resp.Content)); got != tt.wantTokens {
				t.Errorf("reply has %d words, want %d: %q", got, tt.wantTokens, resp.Content)
			}
			want := provider.Usage{PromptTokens: 6, CompletionTokens: tt.wantTokens}
			if resp.Usage != want {
				t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
			}
			if resp.FinishReason != tt.wantFinish {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tt.wantFinish)
			}
			if resp.Model != "mock-1" {
				t.Errorf("Model = %q, want the requested model, mock-1", resp.Model)
			}
		})
	}
}

func TestChatIsRepeatableWithASeed(t *testing.T) {
	reply := func(seed uint64) string {
		resp, err := New("mock", Config{OutputTokens: 20, Seed: seed}).Chat(t.Context(), testRequest(100))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content
	}
	if a, b := reply(7), reply(7); a != b {
		t.Errorf("same seed gave different replies:\n%q\n%q", a, b)
	}
	if a, b := reply(7), reply(8); a == b {
		t.Errorf("different seeds gave the same reply %q", a)
	}
}

func TestChatTakesTheConfiguredTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New("mock", Config{TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond, OutputTokens: 50})
		start := time.Now()
		if _, err := p.Chat(t.Context(), testRequest(5)); err != nil {
			t.Fatal(err)
		}
		// 100ms to the first token, then 5 tokens at 10ms each. Inside the
		// synctest bubble the clock is fake, so this is exact.
		if got, want := time.Since(start), 150*time.Millisecond; got != want {
			t.Errorf("Chat took %v, want %v", got, want)
		}
	})
}

func TestForcedStatus(t *testing.T) {
	tests := []struct {
		status     int
		wantClass  provider.Class
		retryAfter time.Duration
	}{
		{400, provider.BadRequest, 0},
		{401, provider.AuthOrConfig, 0},
		{429, provider.RateLimited, time.Second},
		{500, provider.Unavailable, 0},
		{529, provider.Unavailable, 0},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			_, err := New("mock", Config{OutputTokens: 5, Status: tt.status}).Chat(t.Context(), testRequest(10))
			perr, ok := errors.AsType[*provider.Error](err)
			if !ok {
				t.Fatalf("got %v, want a *provider.Error", err)
			}
			if perr.Class != tt.wantClass || perr.Status != tt.status || perr.RetryAfter != tt.retryAfter {
				t.Errorf("got class %v, status %d, retry after %v; want %v, %d, %v",
					perr.Class, perr.Status, perr.RetryAfter, tt.wantClass, tt.status, tt.retryAfter)
			}
		})
	}
}

func TestErrorRate(t *testing.T) {
	failures := func(rate float64) int {
		p := New("mock", Config{OutputTokens: 1, ErrorRate: rate, Seed: 3})
		n := 0
		for range 1000 {
			if _, err := p.Chat(t.Context(), testRequest(1)); err != nil {
				n++
			}
		}
		return n
	}
	if got := failures(0); got != 0 {
		t.Errorf("error rate 0: %d failures, want 0", got)
	}
	if got := failures(1); got != 1000 {
		t.Errorf("error rate 1: %d failures, want 1000", got)
	}
	// With a fixed seed the count is exact, but any value near 250 is right.
	if got := failures(0.25); got < 200 || got > 300 {
		t.Errorf("error rate 0.25: %d failures in 1000, want about 250", got)
	}
}

func TestHangUntilCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := false
		p := New("mock", Config{OutputTokens: 5, Hang: true})
		p.OnCancel = func() { cancelled = true }

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		start := time.Now()
		_, err := p.Chat(ctx, testRequest(10))

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want context.DeadlineExceeded", err)
		}
		if got := time.Since(start); got != time.Minute {
			t.Errorf("Chat returned after %v, want it to hang for the whole minute", got)
		}
		if !cancelled {
			t.Error("OnCancel was not called")
		}
	})
}

func TestCancelDuringGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New("mock", Config{TTFT: time.Second, OutputTokens: 5})
		seen := make(chan struct{})
		p.OnCancel = func() { close(seen) }

		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error)
		go func() {
			_, err := p.Chat(ctx, testRequest(10))
			errc <- err
		}()

		time.Sleep(100 * time.Millisecond) // fake time: lets Chat start generating
		cancel()
		<-seen
		if err := <-errc; provider.ClassOf(err) != provider.Cancelled {
			t.Errorf("got %v, want a Cancelled error", err)
		}
	})
}

func TestConcurrentCalls(t *testing.T) {
	p := New("mock", Config{OutputTokens: 10, ErrorRate: 0.5})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, _ = p.Chat(t.Context(), testRequest(10))
		})
	}
	wg.Wait() // the race detector checks the shared random source
}
