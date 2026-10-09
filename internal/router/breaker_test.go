package router

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fail records n failures through the breaker, as closed-state calls.
func fail(b *breaker, n int) {
	for range n {
		p, ok := b.allow()
		if ok {
			b.record(p, failure)
		}
	}
}

// Acceptance (M3): the breaker opens, half-opens and closes.
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := newBreaker(3, time.Minute)

	fail(b, 2)
	p, _ := b.allow()
	b.record(p, success) // a success resets the count
	fail(b, 2)
	if b.currentState() != closed {
		t.Fatal("opened after 2 consecutive failures, threshold is 3")
	}
	fail(b, 1)
	if b.currentState() != open {
		t.Fatal("still closed after 3 consecutive failures")
	}
	if _, ok := b.allow(); ok {
		t.Error("an open breaker let a call through")
	}
}

func TestBreakerNeutralOutcomesDontCount(t *testing.T) {
	b := newBreaker(2, time.Minute)
	for range 10 {
		p, _ := b.allow()
		b.record(p, neutral) // rate limits, bad requests, cancellations
	}
	fail(b, 1)
	if b.currentState() != closed {
		t.Error("neutral outcomes counted as failures")
	}
}

func TestBreakerHalfOpenProbe(t *testing.T) {
	tests := []struct {
		name      string
		probe     outcome
		wantState breakerState
	}{
		{"probe succeeds: closes", success, closed},
		{"probe fails: opens again", failure, open},
		{"probe proves nothing: stays half-open", neutral, halfOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newBreaker(1, 30*time.Second)
				fail(b, 1)

				time.Sleep(29 * time.Second)
				if _, ok := b.allow(); ok {
					t.Fatal("allowed a call before the cool-down ended")
				}
				if got := b.retryIn(); got != time.Second {
					t.Errorf("retryIn = %v, want 1s", got)
				}

				time.Sleep(time.Second)
				p, ok := b.allow()
				if !ok || !p.probe {
					t.Fatal("no probe allowed after the cool-down")
				}
				if _, ok := b.allow(); ok {
					t.Error("a second call got through while the probe was in flight")
				}

				b.record(p, tt.probe)
				if got := b.currentState(); got != tt.wantState {
					t.Errorf("state after the probe = %v, want %v", got, tt.wantState)
				}
				if tt.wantState == halfOpen {
					if p, ok := b.allow(); !ok || !p.probe {
						t.Error("after a neutral probe, the next caller should be allowed to probe")
					}
				}
			})
		})
	}
}

func TestBreakerIgnoresLateResults(t *testing.T) {
	b := newBreaker(2, time.Minute)
	// Two calls are let through while closed; both fail and open it.
	p1, _ := b.allow()
	p2, _ := b.allow()
	p3, _ := b.allow()
	b.record(p1, failure)
	b.record(p2, failure)
	// The third call returns late, after the breaker opened. Its success
	// must not close the breaker: only a probe decides that.
	b.record(p3, success)
	if b.currentState() != open {
		t.Errorf("state = %v, want open", b.currentState())
	}
}

// Acceptance (M3): the breaker stays correct under concurrent callers.
// Once the cool-down ends, exactly one of many concurrent callers gets the
// probe, however many race for it.
func TestBreakerOneProbeUnderConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBreaker(1, time.Second)
		fail(b, 1)
		time.Sleep(time.Second)

		var probes atomic.Int32
		var wg sync.WaitGroup
		release := make(chan struct{})
		for range 100 {
			wg.Go(func() {
				p, ok := b.allow()
				if !ok {
					return
				}
				probes.Add(1)
				<-release // hold the probe open while the others try
				b.record(p, success)
			})
		}
		synctest.Wait() // every caller has asked
		if got := probes.Load(); got != 1 {
			t.Errorf("%d callers got through while half-open, want exactly 1", got)
		}
		close(release)
		wg.Wait()
		if b.currentState() != closed {
			t.Errorf("state = %v, want closed after the probe succeeded", b.currentState())
		}
	})
}

// Many goroutines hammering one breaker with mixed outcomes. The race
// detector checks the locking; the test checks that a half-open breaker
// never has two probes in flight.
func TestBreakerConcurrentHammering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBreaker(3, 10*time.Millisecond)
		var inFlightProbes, maxProbes atomic.Int32
		var wg sync.WaitGroup
		for g := range 50 {
			wg.Go(func() {
				for i := range 200 {
					p, ok := b.allow()
					if !ok {
						time.Sleep(time.Millisecond)
						continue
					}
					if p.probe {
						n := inFlightProbes.Add(1)
						for {
							m := maxProbes.Load()
							if n <= m || maxProbes.CompareAndSwap(m, n) {
								break
							}
						}
						time.Sleep(time.Millisecond)
						inFlightProbes.Add(-1)
					}
					b.record(p, []outcome{success, failure, failure, neutral}[(g+i)%4])
				}
			})
		}
		wg.Wait()
		switch got := maxProbes.Load(); {
		case got == 0:
			t.Error("the breaker never reached half-open, so the test checked nothing")
		case got > 1:
			t.Errorf("%d probes were in flight at once, want at most 1", got)
		}
	})
}
