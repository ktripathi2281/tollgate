package limiter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// frozen is a clock that never moves.
func frozen() time.Time { return t0 }

// manual is a clock that moves only when told to.
type manual struct {
	mu  sync.Mutex
	now time.Time
}

func (m *manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *manual) advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
}

// Acceptance (M4): with N goroutines hammering one key under a frozen
// clock, the number admitted is exactly what the bucket allows.
func TestRegistryAdmitsExactlyTheLimitUnderContention(t *testing.T) {
	r := New(frozen)
	l := Limits{RPM: 100, TPM: 10_000}
	var requests, reservations atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Go(func() {
			if _, ok := r.AllowRequest(1, l); ok {
				requests.Add(1)
			}
			if _, ok := r.ReserveTokens(1, l, 300); ok {
				reservations.Add(1)
			}
		})
	}
	wg.Wait()
	if n := requests.Load(); n != 100 {
		t.Errorf("admitted %d requests, want exactly 100", n)
	}
	if n := reservations.Load(); n != 33 {
		t.Errorf("admitted %d reservations of 300 tokens, want exactly 33 (10,000 / 300)", n)
	}
}

func TestRegistryRetryAfter(t *testing.T) {
	r := New(frozen)
	l := Limits{RPM: 2, TPM: 600}
	r.AllowRequest(1, l)
	r.AllowRequest(1, l)
	if wait, ok := r.AllowRequest(1, l); ok || wait != 30*time.Second {
		t.Errorf("third request at 2 a minute = %v, %v; want refused, retry in 30s", wait, ok)
	}
	r.ReserveTokens(1, l, 600)
	if wait, ok := r.ReserveTokens(1, l, 100); ok || wait != 10*time.Second {
		t.Errorf("100 tokens at 600 a minute from empty = %v, %v; want refused, retry in 10s", wait, ok)
	}
}

func TestRegistryKeysAreIndependent(t *testing.T) {
	r := New(frozen)
	l := Limits{RPM: 1, TPM: 100}
	if _, ok := r.AllowRequest(1, l); !ok {
		t.Fatal("key 1's first request refused")
	}
	if _, ok := r.AllowRequest(1, l); ok {
		t.Fatal("key 1's second request allowed")
	}
	if _, ok := r.AllowRequest(2, l); !ok {
		t.Error("key 2 is limited by key 1's use")
	}
}

func TestRegistryRefunds(t *testing.T) {
	r := New(frozen)
	l := Limits{RPM: 10, TPM: 1000}

	if _, ok := r.ReserveTokens(1, l, 1000); !ok {
		t.Fatal("reserving the whole limit failed")
	}
	r.RefundTokens(1, l, 1000, 400) // used 400 of 1,000: 600 come back
	if _, ok := r.ReserveTokens(1, l, 600); !ok {
		t.Error("the 600 unused tokens weren't refunded")
	}
	if _, ok := r.ReserveTokens(1, l, 1); ok {
		t.Error("more than the refund came back")
	}

	// A refund never fills the bucket beyond its capacity.
	r.RefundTokens(2, l, 500, 0)
	if _, ok := r.ReserveTokens(2, l, 1000); !ok {
		t.Fatal("a fresh key's full allowance is missing")
	}
	if _, ok := r.ReserveTokens(2, l, 1); ok {
		t.Error("a refund on a full bucket raised it above the capacity")
	}
}

func TestRegistryChargesOveruse(t *testing.T) {
	clk := &manual{now: t0}
	r := New(clk.Now)
	l := Limits{RPM: 10, TPM: 600} // 10 tokens a second
	r.ReserveTokens(1, l, 100)
	r.RefundTokens(1, l, 100, 700) // used 600 more than reserved: 500 - 600 = -100
	if wait, ok := r.ReserveTokens(1, l, 100); ok || wait != 20*time.Second {
		t.Errorf("after overuse = %v, %v; want refused, retry in 20s (from -100 to 100)", wait, ok)
	}
	clk.advance(20 * time.Second)
	if _, ok := r.ReserveTokens(1, l, 100); !ok {
		t.Error("after 20s the reservation should fit")
	}
}

func TestRegistryRejectsReservationsLargerThanTheLimit(t *testing.T) {
	r := New(frozen)
	if wait, ok := r.ReserveTokens(1, Limits{RPM: 1, TPM: 100}, 101); ok || wait != 0 {
		t.Errorf("ReserveTokens(101) with TPM 100 = %v, %v; want false with no wait", wait, ok)
	}
	if _, ok := r.ReserveTokens(1, Limits{RPM: 1, TPM: 100}, 0); !ok {
		t.Error("reserving nothing should always succeed")
	}
}

func TestRegistryAppliesChangedLimits(t *testing.T) {
	r := New(frozen)
	r.AllowRequest(1, Limits{RPM: 5, TPM: 100})
	// The key's limit drops to 1 a minute: its 4 remaining requests shrink to 1.
	low := Limits{RPM: 1, TPM: 100}
	if _, ok := r.AllowRequest(1, low); !ok {
		t.Fatal("first request after lowering the limit refused")
	}
	if _, ok := r.AllowRequest(1, low); ok {
		t.Error("second request allowed beyond the new limit")
	}
}

func TestRegistryDropsKeysThatHaveRefilled(t *testing.T) {
	clk := &manual{now: t0}
	r := New(clk.Now)
	l := Limits{RPM: 60, TPM: 600}
	r.AllowRequest(1, l)       // key 1 is used a little
	r.ReserveTokens(2, l, 600) // key 2 empties its token bucket
	r.RefundTokens(2, l, 600, 1200)

	// A minute later key 1 has refilled; key 2 is still 600 short.
	clk.advance(time.Minute)
	r.AllowRequest(3, l) // any call sweeps
	if n := r.size(); n != 2 {
		t.Errorf("%d keys held after the sweep, want 2 (key 2 still refilling, key 3 new)", n)
	}

	// Dropping key 1 changed nothing: it gets a full minute's allowance.
	for i := range 60 {
		if _, ok := r.AllowRequest(1, l); !ok {
			t.Fatalf("request %d of key 1 refused after it was dropped", i+1)
		}
	}
}
