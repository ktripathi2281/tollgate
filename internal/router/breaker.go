package router

import (
	"sync"
	"time"
)

// breakerState is where a circuit breaker is in its cycle.
type breakerState int

const (
	// closed: the target works; calls go through, and consecutive
	// Unavailable failures are counted.
	closed breakerState = iota
	// open: the target failed too often; calls are skipped until the
	// cool-down ends.
	open
	// halfOpen: the cool-down is over; one probe call may go through, and
	// its result decides between closed and open.
	halfOpen
)

func (s breakerState) String() string {
	switch s {
	case closed:
		return "closed"
	case open:
		return "open"
	default:
		return "half_open"
	}
}

// outcome is what a call tells the breaker.
type outcome int

const (
	success outcome = iota // the target answered
	failure                // the target was Unavailable
	neutral                // anything else: says nothing about the target's health
)

// permit is the breaker's answer to allow. A probe permit belongs to the
// single call allowed through while half-open.
type permit struct{ probe bool }

// breaker is a circuit breaker for one target. A mutex guards its state, so
// any number of concurrent requests can share it.
type breaker struct {
	threshold int           // consecutive failures that open it
	openFor   time.Duration // how long it stays open before a probe

	mu       sync.Mutex
	state    breakerState
	failures int       // consecutive failures, while closed
	openedAt time.Time // when it last opened
	probing  bool      // the half-open probe is in flight
}

func newBreaker(threshold int, openFor time.Duration) *breaker {
	return &breaker{threshold: threshold, openFor: openFor}
}

// allow reports whether a call may go to the target now. Once the cool-down
// is over it lets exactly one caller through as the probe; everyone else is
// refused until that probe reports back.
func (b *breaker) allow() (permit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case closed:
		return permit{}, true
	case open:
		if time.Since(b.openedAt) < b.openFor {
			return permit{}, false
		}
		b.state = halfOpen
	}
	// halfOpen
	if b.probing {
		return permit{}, false
	}
	b.probing = true
	return permit{probe: true}, true
}

// record reports how a call ended. p is the permit allow gave it, or the
// zero permit for a failure noticed later, such as a stream failing after
// it was handed over.
func (b *breaker) record(p permit, o outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.probe {
		b.probing = false
		switch o {
		case success:
			b.state, b.failures = closed, 0
		case failure:
			b.trip()
		case neutral:
			// The probe proved nothing, such as a client that went away.
			// Stay half-open so the next caller can probe.
		}
		return
	}
	// A call let through while closed. If the breaker has opened since,
	// a late result changes nothing: only the probe decides.
	if b.state != closed {
		return
	}
	switch o {
	case success:
		b.failures = 0
	case failure:
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	}
}

// trip opens the breaker. The caller holds mu.
func (b *breaker) trip() {
	b.state, b.failures, b.openedAt = open, 0, time.Now()
}

// retryIn is how long until an open breaker lets a probe through; 0 if it
// isn't open.
func (b *breaker) retryIn() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != open {
		return 0
	}
	return max(0, b.openFor-time.Since(b.openedAt))
}

// currentState returns the breaker's state, for tests and, from M6, metrics.
func (b *breaker) currentState() breakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
