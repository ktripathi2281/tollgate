package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The batch tests are for the M6 exercise, batch.

// collector records the batches flush receives.
type collector struct {
	mu      sync.Mutex
	batches [][]Entry
}

func (c *collector) flush(b []Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batches = append(c.batches, b)
}

// sizes returns the size of each batch so far.
func (c *collector) sizes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var s []int
	for _, b := range c.batches {
		s = append(s, len(b))
	}
	return s
}

func entry(i int) Entry { return Entry{RequestID: fmt.Sprintf("req_%d", i)} }

// Acceptance (M6): the worker flushes by size.
func TestBatchFlushesBySize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		in := make(chan Entry, 100)
		var c collector
		done := make(chan struct{})
		go func() { batch(ctx, in, 3, time.Hour, c.flush); close(done) }()

		for i := range 7 {
			in <- entry(i)
		}
		synctest.Wait()
		if got := c.sizes(); fmt.Sprint(got) != "[3 3]" {
			t.Fatalf("batches %v, want two full batches of 3 and one left waiting", got)
		}
		cancel()
		<-done
		if got := c.sizes(); fmt.Sprint(got) != "[3 3 1]" {
			t.Errorf("after shutdown: batches %v, want the last entry flushed too", got)
		}
	})
}

// Acceptance (M6): the worker flushes by time.
func TestBatchFlushesByTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		in := make(chan Entry, 100)
		var c collector
		done := make(chan struct{})
		go func() { batch(ctx, in, 100, time.Second, c.flush); close(done) }()

		in <- entry(1)
		in <- entry(2)
		synctest.Wait()
		if got := c.sizes(); len(got) != 0 {
			t.Fatalf("flushed %v before the interval, want nothing yet", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := c.sizes(); fmt.Sprint(got) != "[2]" {
			t.Fatalf("after the interval: batches %v, want [2]", got)
		}
		// Ticks with nothing waiting flush nothing.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := c.sizes(); fmt.Sprint(got) != "[2]" {
			t.Errorf("idle ticks: batches %v, want no empty flushes", got)
		}
		cancel()
		<-done
	})
}

// Acceptance (M6): the worker drains on shutdown.
func TestBatchDrainsOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // shut down before it even starts
		in := make(chan Entry, 100)
		for i := range 8 {
			in <- entry(i)
		}
		var c collector
		batch(ctx, in, 3, time.Hour, c.flush)

		if got := c.sizes(); fmt.Sprint(got) != "[3 3 2]" {
			t.Fatalf("batches %v, want everything queued flushed: [3 3 2]", got)
		}
		var order []string
		for _, b := range c.batches {
			for _, e := range b {
				order = append(order, e.RequestID)
			}
		}
		if fmt.Sprint(order) != "[req_0 req_1 req_2 req_3 req_4 req_5 req_6 req_7]" {
			t.Errorf("entries out of order: %v", order)
		}
	})
}

func TestBatchGivesEachFlushItsOwnSlice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		in := make(chan Entry, 10)
		for i := range 4 {
			in <- entry(i)
		}
		var kept [][]Entry
		batch(ctx, in, 2, time.Hour, func(b []Entry) {
			kept = append(kept, b)
		})
		if len(kept) != 2 || kept[0][0].RequestID != "req_0" || kept[1][0].RequestID != "req_2" {
			t.Errorf("batches %v: a later batch overwrote an earlier one", kept)
		}
	})
}

// Acceptance (M6): drops are counted when the queue is full.
func TestRecordDropsWhenTheQueueIsFull(t *testing.T) {
	r := NewRecorder(nil, 2, 10, time.Second, slog.New(slog.DiscardHandler))
	var queued int
	for i := range 5 {
		if r.Record(entry(i)) {
			queued++
		}
	}
	if queued != 2 || r.Dropped.Load() != 3 || r.Queued() != 2 {
		t.Errorf("queued %d, dropped %d, waiting %d; want 2, 3, 2", queued, r.Dropped.Load(), r.Queued())
	}
}

// stalledWriter blocks every write until released, like a database that
// has stopped answering.
type stalledWriter struct {
	release chan struct{}
	mu      sync.Mutex
	rows    int
}

func (w *stalledWriter) WriteRequestLogs(ctx context.Context, entries []Entry) error {
	select {
	case <-w.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rows += len(entries)
	return nil
}

// Acceptance (M6): recording never blocks, even with the database stalled.
func TestRecordDoesNotWaitForAStalledDatabase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &stalledWriter{release: make(chan struct{})}
		r := NewRecorder(w, 10, 5, time.Second, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { r.Run(ctx); close(done) }()

		// A full batch, which the worker takes and gets stuck writing.
		for i := range 5 {
			r.Record(entry(i))
		}
		synctest.Wait()

		start := time.Now()
		for i := range 100 {
			r.Record(entry(i))
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("100 Record calls took %v of fake time, want none: Record must not wait", elapsed)
		}
		// The queue took 10; the other 90 were dropped.
		if r.Dropped.Load() != 90 || r.Queued() != 10 {
			t.Errorf("dropped %d with %d queued, want 90 and 10", r.Dropped.Load(), r.Queued())
		}

		close(w.release)
		cancel()
		<-done
		if r.Written.Load() != 15 || w.rows != 15 {
			t.Errorf("written %d (writer saw %d), want 15: the held batch and the drained queue", r.Written.Load(), w.rows)
		}
	})
}

type failingWriter struct{}

func (failingWriter) WriteRequestLogs(context.Context, []Entry) error {
	return errors.New("connection refused")
}

func TestFailedWritesAreCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewRecorder(failingWriter{}, 10, 2, time.Second, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r.Record(entry(1))
		r.Record(entry(2))
		r.Record(entry(3))
		r.Run(ctx)
		if r.Failed.Load() != 3 || r.Written.Load() != 0 {
			t.Errorf("failed %d, written %d; want 3, 0", r.Failed.Load(), r.Written.Load())
		}
	})
}
