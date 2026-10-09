package router

import (
	"math/rand/v2"
	"time"
)

// backoff returns how long to wait before retry number n of a target, where
// n is 1 for the first retry. The wait grows exponentially, base, 2×base,
// 4×base and so on, up to max. Then full jitter picks the actual wait at
// random between 0 and that cap, so that clients who failed together don't
// all retry together. jitter(d) returns a random duration from 0 to d; tests
// pass a fixed one. An n below 1 is treated as 1.
func backoff(n int, base, max time.Duration, jitter func(time.Duration) time.Duration) time.Duration {
	// EXERCISE: implement backoff; backoff_test.go has every case it must pass.
	// Hint: double base n-1 times, but stop as soon as it reaches max, because
	// shifting first can overflow for a large n. Then return jitter(capped).
	_, _, _, _ = n, base, max, jitter
	return 0
}

// fullJitter returns a uniformly random duration from 0 to d.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
