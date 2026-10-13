package usage

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ktripathi2281/tollgate/internal/money"
)

// Entry is one chat request's row in the request log (brief section 10).
type Entry struct {
	Time      time.Time // when the request arrived
	RequestID string
	KeyID     int64  // 0 if the request had no valid key
	Alias     string // the model the client asked for; "" if the body wasn't read
	// Provider and Model are the target that served the request, or the
	// last one tried; "" if no provider was called.
	Provider         string
	Model            string
	Stream           bool
	Status           int
	ErrorCode        string // the error code sent to the client; "" if none
	Attempts         int
	PromptTokens     int
	CompletionTokens int
	UsageEstimated   bool
	Cost             money.Micros
	Latency          time.Duration
	TTFT             time.Duration // time to the first content; 0 unless a stream sent some
	Replayed         bool
}

// Writer stores a batch of entries. store.Store implements it.
type Writer interface {
	WriteRequestLogs(ctx context.Context, entries []Entry) error
}

// writeTimeout bounds one batch write.
const writeTimeout = 10 * time.Second

// Recorder queues entries for one worker that writes them in batches.
// Record never waits: when the queue is full, the entry is dropped and
// counted. Nothing about money depends on these rows; budgets have their
// own tables.
type Recorder struct {
	queue      chan Entry
	writer     Writer
	batchSize  int
	flushEvery time.Duration
	log        *slog.Logger

	// Dropped counts entries refused because the queue was full; Written
	// counts entries stored; Failed counts entries lost because a write
	// failed. They are exported as metrics.
	Dropped atomic.Int64
	Written atomic.Int64
	Failed  atomic.Int64
}

// NewRecorder returns a Recorder that queues up to buffer entries and
// writes at most batchSize at a time, waiting at most flushEvery to fill a
// batch.
func NewRecorder(w Writer, buffer, batchSize int, flushEvery time.Duration, log *slog.Logger) *Recorder {
	return &Recorder{
		queue: make(chan Entry, buffer), writer: w,
		batchSize: batchSize, flushEvery: flushEvery, log: log,
	}
}

// Record queues e for writing and reports whether it was queued. It never
// blocks, so a slow or stalled database can't slow requests down.
func (r *Recorder) Record(e Entry) bool {
	select {
	case r.queue <- e:
		return true
	default:
		r.Dropped.Add(1)
		return false
	}
}

// Queued returns how many entries are waiting to be written.
func (r *Recorder) Queued() int {
	return len(r.queue)
}

// Run writes queued entries in batches until ctx is done, then writes
// whatever is still queued, and returns. Run it once, in a goroutine of its
// own, and cancel ctx only after the server has stopped handling requests,
// so the final drain sees every entry.
func (r *Recorder) Run(ctx context.Context) {
	batch(ctx, r.queue, r.batchSize, r.flushEvery, r.write)
}

// write stores one batch. Its context is its own, so the final drain still
// writes after Run's context is done.
func (r *Recorder) write(entries []Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := r.writer.WriteRequestLogs(ctx, entries); err != nil {
		r.Failed.Add(int64(len(entries)))
		r.log.Error("writing request logs failed; the rows are lost", "rows", len(entries), "error", err)
		return
	}
	r.Written.Add(int64(len(entries)))
}

// batch hands the entries arriving on in to flush, in batches: as soon as
// a batch has size entries, and, every interval, whatever smaller batch is
// waiting. When ctx is done, it takes what is already queued in in, flushes
// it, and returns. flush never gets an empty batch, and each batch is a
// slice of its own.
func batch(ctx context.Context, in <-chan Entry, size int, interval time.Duration, flush func([]Entry)) {
	// EXERCISE: implement batch; requestlog_test.go has every case it must pass.
	// Hint: one select over in, a time.Ticker and ctx.Done(). When ctx is
	// done, drain in with a select that has a default case, then flush.
	_, _, _, _ = in, size, interval, flush
	<-ctx.Done() // until it's written, no request-log row is ever written
}
