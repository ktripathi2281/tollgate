package limiter

import (
	"testing"
	"time"
)

// These tests are for the M4 exercise, bucket.take.

var t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func TestTakeSpendsAFullBucketThenWaits(t *testing.T) {
	b := newBucket(60, t0) // 60 a minute: one a second
	for i := range 60 {
		if _, ok := b.take(t0, 1); !ok {
			t.Fatalf("take %d of 60 from a full bucket failed", i+1)
		}
	}
	wait, ok := b.take(t0, 1)
	if ok || wait != time.Second {
		t.Errorf("take from an empty bucket = %v, %v; want false and 1s", wait, ok)
	}
}

func TestTakeRefillsFirst(t *testing.T) {
	b := newBucket(60, t0)
	if _, ok := b.take(t0, 60); !ok {
		t.Fatal("taking the whole capacity failed")
	}
	if _, ok := b.take(t0.Add(2*time.Second), 2); !ok {
		t.Error("two seconds later, two tokens should be there")
	}
	if _, ok := b.take(t0.Add(2*time.Second), 1); ok {
		t.Error("and no more")
	}
	// Refilling stops at the capacity: an hour idle is still one minute's worth.
	if _, ok := b.take(t0.Add(time.Hour), 60); !ok {
		t.Error("after an hour the bucket should be full")
	}
	if _, ok := b.take(t0.Add(time.Hour), 1); ok {
		t.Error("but no fuller than the capacity")
	}
}

func TestTakeWaitIsForTheMissingPart(t *testing.T) {
	b := newBucket(60, t0)
	b.take(t0, 60)
	// Half a second later there is half a token, so 3 tokens are 2.5s away.
	wait, ok := b.take(t0.Add(500*time.Millisecond), 3)
	if ok || wait != 2500*time.Millisecond {
		t.Errorf("take(3) = %v, %v; want false and 2.5s", wait, ok)
	}
}

func TestFailedTakeTakesNothing(t *testing.T) {
	b := newBucket(10, t0)
	b.take(t0, 8)
	if _, ok := b.take(t0, 5); ok {
		t.Fatal("take(5) with 2 left succeeded")
	}
	if _, ok := b.take(t0, 2); !ok {
		t.Error("the failed take(5) must leave the 2 tokens in place")
	}
}

func TestTakeWaitRoundsUp(t *testing.T) {
	b := newBucket(7, t0) // one token every 60/7 s, which isn't a whole number of nanoseconds
	b.take(t0, 7)
	wait, ok := b.take(t0, 1)
	if ok {
		t.Fatal("take from an empty bucket succeeded")
	}
	if wait != 8571428572*time.Nanosecond {
		t.Errorf("wait = %v (%d ns), want 8571428572 ns: rounded up, so waiting that long is enough", wait, wait.Nanoseconds())
	}
	if _, ok := b.take(t0.Add(wait), 1); !ok {
		t.Error("after the returned wait, the token should be there")
	}
}

func TestTakeAfterOveruse(t *testing.T) {
	b := newBucket(60, t0)
	b.give(t0, -90) // a request used 90 more tokens than it reserved: 60 - 90 = -30
	wait, ok := b.take(t0, 1)
	if ok || wait != 31*time.Second {
		t.Errorf("take(1) from -30 = %v, %v; want false and 31s", wait, ok)
	}
}
