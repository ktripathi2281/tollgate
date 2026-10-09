package router

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// ChatStream opens a stream on the alias's targets, with retries and
// fallback, and reads up to the commit point: the first content, a finish,
// or the end of the stream.
//
// A failure before the commit point, on any attempt, is treated like a
// failed call: the router retries or falls back, and if everything fails
// the caller gets an error while the client has seen nothing. Once
// ChatStream returns a Stream, the stream is committed: there is no more
// retry, and the caller must Close it.
func (r *Router) ChatStream(ctx context.Context, alias *Alias, req provider.ChatRequest) (*Stream, Result, error) {
	var res Result
	if err := r.validate(alias, &req); err != nil {
		return nil, res, err
	}
	// The total deadline covers every attempt and the whole stream, so the
	// committed Stream takes over its cancel function.
	ctx, cancelTotal := context.WithTimeoutCause(ctx, r.timeouts.Total, ErrTotalTimeout)

	var committed *Stream
	err := r.run(ctx, alias, &res, func(ctx context.Context, t *Target) (outcome, error) {
		s, err := r.open(ctx, t, req)
		if err != nil {
			return breakerOutcome(err), err
		}
		committed = s
		return success, nil
	})
	if err != nil {
		cancelTotal()
		return nil, res, err
	}
	committed.cancelTotal = cancelTotal
	return committed, res, nil
}

// open makes one streaming attempt at a target and reads it to the commit
// point. Each attempt gets its own context and watchdog, so a timed-out
// attempt can be abandoned and the next one started cleanly.
func (r *Router) open(ctx context.Context, t *Target, req provider.ChatRequest) (*Stream, error) {
	req.Model = t.Model
	ctx, cancelStream := context.WithCancelCause(ctx)
	s := &Stream{
		ctx:          ctx,
		cancelStream: cancelStream,
		cancelTotal:  func() {},
		idle:         r.timeouts.Idle,
		model:        t.Model,
		breaker:      t.breaker,
	}
	// Until the model produces output, the watchdog enforces the first-token
	// timeout. It cancels the context instead of racing a goroutine against
	// Next, so a stream needs no goroutine of its own.
	s.watchdog = time.AfterFunc(r.timeouts.FirstToken, func() { cancelStream(ErrFirstTokenTimeout) })

	src, err := t.Provider.ChatStream(ctx, &req)
	if err != nil {
		err = timeoutCause(ctx, err)
		s.Close()
		return nil, err
	}
	s.src = src
	if err := s.readToCommit(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Stream is a committed stream with the gateway's timeouts applied. Next
// and Close are called from one goroutine.
type Stream struct {
	src          provider.Stream
	ctx          context.Context
	cancelStream context.CancelCauseFunc
	cancelTotal  context.CancelFunc
	watchdog     *time.Timer // cancels ctx when the next chunk is late
	idle         time.Duration
	model        string
	breaker      *breaker // the target's, to report a failure after commit

	producing bool              // the model has produced output, visible or not
	pending   []*provider.Chunk // read before the commit point, not yet returned
	ended     bool              // the upstream stream is over
}

// readToCommit reads until the commit point and keeps what it read for
// Next. Keepalives are dropped. Reasoning is dropped too, but it shows the
// model is working, so it ends the first-token timeout.
func (s *Stream) readToCommit() error {
	for {
		chunk, err := s.read()
		if errors.Is(err, io.EOF) {
			s.ended = true
			return nil
		}
		if err != nil {
			return err
		}
		s.onChunk(chunk)
		if chunk.IsKeepalive() {
			continue
		}
		s.pending = append(s.pending, chunk)
		if chunk.Delta != "" || chunk.FinishReason != "" {
			return nil
		}
	}
}

// onChunk updates the watchdog for a chunk that just arrived. Before the
// model has produced anything, only output (text, reasoning, a finish or
// usage) stops the first-token timer; a keepalive doesn't, because it
// doesn't show the model is producing. From the first output on, the
// watchdog enforces the idle timeout, and every chunk restarts it.
func (s *Stream) onChunk(c *provider.Chunk) {
	output := c.Reasoning || !c.IsKeepalive()
	switch {
	case !s.producing && output:
		s.producing = true
		// A new timer, because Reset would reschedule the old function,
		// which reports a first-token timeout.
		s.watchdog.Stop()
		s.watchdog = time.AfterFunc(s.idle, func() { s.cancelStream(ErrIdleTimeout) })
	case s.producing:
		s.watchdog.Reset(s.idle)
	}
}

// Next returns the next chunk that carries data, or io.EOF at the end. It
// skips keepalives and reasoning, but each one restarts the idle timeout. A
// failure counts against the target's breaker when the target was
// Unavailable.
func (s *Stream) Next() (*provider.Chunk, error) {
	if len(s.pending) > 0 {
		chunk := s.pending[0]
		s.pending = s.pending[1:]
		return chunk, nil
	}
	for !s.ended {
		chunk, err := s.read()
		if errors.Is(err, io.EOF) {
			s.ended = true
			break
		}
		if err != nil {
			s.ended = true
			if provider.ClassOf(err) == provider.Unavailable {
				s.breaker.record(permit{}, failure)
			}
			return nil, err
		}
		s.onChunk(chunk)
		if !chunk.IsKeepalive() {
			return chunk, nil
		}
	}
	return nil, io.EOF
}

// read returns the provider's next chunk, notes the upstream model if the
// chunk reports it, and turns a context error into the timeout behind it.
func (s *Stream) read() (*provider.Chunk, error) {
	chunk, err := s.src.Next()
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	if err != nil {
		return nil, timeoutCause(s.ctx, err)
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	return chunk, nil
}

// Model is the upstream model serving the stream: what the provider
// reported, or the target's model ID if it reported nothing.
func (s *Stream) Model() string {
	return s.model
}

// Close ends the stream: it stops the watchdog, cancels the upstream call
// if it is still running, and releases the provider's stream. It is safe to
// call more than once.
func (s *Stream) Close() error {
	s.watchdog.Stop()
	s.cancelStream(nil)
	s.cancelTotal()
	if s.src == nil {
		return nil
	}
	return s.src.Close()
}
