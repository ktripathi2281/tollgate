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
	d := min(base, max)
	// Double once per retry after the first, stopping at max. Shifting base
	// left by n-1 would overflow for a large n, and comparing with max/2
	// before doubling means d*2 never can.
	for i := 1; i < n && d < max; i++ {
		if d > max/2 {
			d = max
		} else {
			d *= 2
		}
	}
	return jitter(d)
}

// fullJitter returns a uniformly random duration from 0 to d.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
