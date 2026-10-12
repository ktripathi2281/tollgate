package limiter

import (
	"sync"
	"time"
)

// Limits are one key's rates. Both must be positive.
type Limits struct {
	RPM int // requests per minute
	TPM int // tokens per minute
}

// sweepEvery is how often the registry drops buckets that have refilled.
const sweepEvery = time.Minute

// Registry holds one pair of buckets per key, created on first use. It
// keeps everything in this process's memory, so limits apply per gateway
// instance.
//
// One mutex guards everything. Each operation is a few arithmetic steps,
// so the lock is held for well under a microsecond.
type Registry struct {
	now func() time.Time

	mu        sync.Mutex
	keys      map[int64]*pair
	lastSweep time.Time
}

// pair is one key's buckets, and the limits they were built with.
type pair struct {
	limits   Limits
	requests *bucket
	tokens   *bucket
}

// New returns an empty Registry. now is the clock; tests pass a fake one.
func New(now func() time.Time) *Registry {
	return &Registry{now: now, keys: make(map[int64]*pair)}
}

// AllowRequest counts one request against the key's requests per minute.
// If the limit is reached, it returns false and how long until a request
// would be allowed.
func (r *Registry) AllowRequest(keyID int64, l Limits) (retryAfter time.Duration, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	return r.pair(keyID, l, now).requests.take(now, 1)
}

// ReserveTokens takes n tokens from the key's tokens per minute before a
// request runs. If they aren't there, it takes none and returns false and
// how long until they will be. n more than l.TPM can never fit, so it
// returns false with no wait; callers should reject such requests first.
func (r *Registry) ReserveTokens(keyID int64, l Limits, n int) (retryAfter time.Duration, ok bool) {
	if n > l.TPM {
		return 0, false
	}
	if n <= 0 {
		return 0, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	return r.pair(keyID, l, now).tokens.take(now, float64(n))
}

// RefundTokens settles a reservation once the request has finished: it
// gives back reserved minus used. If the request used more than it
// reserved, the difference is taken instead.
func (r *Registry) RefundTokens(keyID int64, l Limits, reserved, used int) {
	if reserved == used {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pair(keyID, l, now).tokens.give(now, float64(reserved-used))
}

// pair returns the key's buckets, creating them on first use and resizing
// them if the key's limits have changed. Now and then it first drops the
// pairs that have refilled. r.mu must be held.
func (r *Registry) pair(keyID int64, l Limits, now time.Time) *pair {
	if now.Sub(r.lastSweep) >= sweepEvery {
		r.sweep(now)
	}
	p, ok := r.keys[keyID]
	switch {
	case !ok:
		p = &pair{limits: l, requests: newBucket(l.RPM, now), tokens: newBucket(l.TPM, now)}
		r.keys[keyID] = p
	case p.limits != l:
		p.requests.resize(now, l.RPM)
		p.tokens.resize(now, l.TPM)
		p.limits = l
	}
	return p
}

// sweep drops the pairs whose buckets are both full. A key seen again gets
// a new pair, which starts full, so dropping one changes no decision; it
// only frees the memory of keys that have gone quiet. r.mu must be held.
func (r *Registry) sweep(now time.Time) {
	for id, p := range r.keys {
		if p.requests.full(now) && p.tokens.full(now) {
			delete(r.keys, id)
		}
	}
	r.lastSweep = now
}

// size returns the number of keys with buckets, for tests.
func (r *Registry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.keys)
}
