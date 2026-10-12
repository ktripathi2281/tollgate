// Package limiter enforces per-key rate limits with token buckets: one for
// requests per minute, and one for tokens per minute.
package limiter

import (
	"math"
	"time"
)

// bucket is a token bucket. It holds up to capacity tokens and refills at
// capacity tokens per minute, so a key can spend a minute's allowance at
// once and then continue at the steady rate. It starts full.
//
// tokens can go below zero: when a request turns out to have used more
// tokens than it reserved, the difference is taken afterwards, and the
// bucket refills from there.
//
// Token counts are float64 because refilling earns fractions of a token.
// This is a rate limit, not money: no amount is stored or billed from it.
type bucket struct {
	capacity float64
	perSec   float64 // refill rate
	tokens   float64
	last     time.Time // when tokens was last brought up to date
}

// newBucket returns a full bucket that allows perMinute tokens a minute.
// perMinute must be positive.
func newBucket(perMinute int, now time.Time) *bucket {
	c := float64(perMinute)
	return &bucket{capacity: c, perSec: c / 60, tokens: c, last: now}
}

// refill adds the tokens earned since b.last, up to the capacity. A clock
// that went backwards earns nothing.
func (b *bucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed.Seconds()*b.perSec)
		b.last = now
	}
}

// take removes n tokens if the bucket has them, after refilling it. If it
// doesn't, it removes nothing and returns how long until it will have
// them, assuming nothing else takes any first. n must be positive and no
// more than the capacity.
func (b *bucket) take(now time.Time, n float64) (wait time.Duration, ok bool) {
	b.refill(now)
	if b.tokens >= n {
		b.tokens -= n
		return 0, true
	}
	// Rounding up means that waiting exactly this long is always enough.
	missing := n - b.tokens
	return time.Duration(math.Ceil(missing / b.perSec * float64(time.Second))), false
}

// give returns n tokens to the bucket, up to its capacity. A negative n
// takes tokens, and can leave the bucket below zero.
func (b *bucket) give(now time.Time, n float64) {
	b.refill(now)
	b.tokens = min(b.capacity, b.tokens+n)
}

// full reports whether the bucket is full at now. A full bucket is in the
// same state as a new one, so it can be dropped and recreated later.
func (b *bucket) full(now time.Time) bool {
	b.refill(now)
	return b.tokens >= b.capacity
}

// resize changes the bucket's rate, keeping its tokens up to the new
// capacity.
func (b *bucket) resize(now time.Time, perMinute int) {
	b.refill(now)
	b.capacity = float64(perMinute)
	b.perSec = b.capacity / 60
	b.tokens = min(b.capacity, b.tokens)
}
