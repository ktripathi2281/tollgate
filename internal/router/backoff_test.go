package router

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// These tests are for the M3 exercise, backoff.

// noJitter returns the cap itself, so the tests see the exponential curve.
func noJitter(d time.Duration) time.Duration { return d }

func TestBackoffGrowsExponentiallyToTheCap(t *testing.T) {
	const base, max = 100 * time.Millisecond, time.Second
	tests := []struct {
		n    int
		want time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, time.Second}, // 1.6s, capped
		{6, time.Second},
		{0, 100 * time.Millisecond},  // below 1 counts as 1
		{-3, 100 * time.Millisecond}, // below 1 counts as 1
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.n), func(t *testing.T) {
			if got := backoff(tt.n, base, max, noJitter); got != tt.want {
				t.Errorf("backoff(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

// A large retry number must not overflow into a negative or tiny wait.
func TestBackoffDoesNotOverflow(t *testing.T) {
	for _, n := range []int{40, 63, 64, 65, 1000, math.MaxInt} {
		if got := backoff(n, 200*time.Millisecond, 2*time.Second, noJitter); got != 2*time.Second {
			t.Errorf("backoff(%d) = %v, want the cap, 2s", n, got)
		}
	}
	// A cap near the largest duration still caps.
	if got := backoff(100, time.Second, time.Duration(math.MaxInt64), noJitter); got != time.Duration(math.MaxInt64) {
		t.Errorf("with a huge cap: got %v", got)
	}
}

func TestBackoffBaseAboveCap(t *testing.T) {
	if got := backoff(1, 5*time.Second, time.Second, noJitter); got != time.Second {
		t.Errorf("backoff with base 5s and cap 1s = %v, want 1s", got)
	}
}

// Full jitter: the wait is jitter applied to the capped value, and nothing
// else.
func TestBackoffAppliesJitterToTheCap(t *testing.T) {
	var asked []time.Duration
	half := func(d time.Duration) time.Duration {
		asked = append(asked, d)
		return d / 2
	}
	if got := backoff(3, 100*time.Millisecond, time.Second, half); got != 200*time.Millisecond {
		t.Errorf("backoff(3) with half jitter = %v, want 200ms", got)
	}
	if got := backoff(9, 100*time.Millisecond, time.Second, half); got != 500*time.Millisecond {
		t.Errorf("backoff(9) with half jitter = %v, want 500ms", got)
	}
	if len(asked) != 2 || asked[0] != 400*time.Millisecond || asked[1] != time.Second {
		t.Errorf("jitter was asked about %v, want [400ms 1s]: once per call, with the capped value", asked)
	}
}

func TestFullJitterStaysInRange(t *testing.T) {
	for range 1000 {
		if d := fullJitter(time.Second); d < 0 || d > time.Second {
			t.Fatalf("fullJitter(1s) = %v, out of range", d)
		}
	}
	if d := fullJitter(0); d != 0 {
		t.Errorf("fullJitter(0) = %v, want 0", d)
	}
}
