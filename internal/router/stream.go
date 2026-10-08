package router

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// ChatStream opens a stream to the alias's target and reads up to the
// commit point: the first content, a finish, or the end of the stream.
//
// A failure before the commit point is returned as an error, while the
// client has seen nothing, so the caller can still send an ordinary error
// response (and from M3 the router can retry or fall back). Once ChatStream
// returns a Stream, the stream is committed: there is no more retry, and
// the caller must Close it.
func (r *Router) ChatStream(ctx context.Context, alias *Alias, req provider.ChatRequest) (*Stream, Result, error) {
	t := alias.Targets[0]
	req.Model = t.Model
	res := Result{Provider: t.Provider.Name(), Model: t.Model, Attempts: 1}

	// The total deadline covers the whole stream, so the Stream owns its
	// cancel function rather than a defer here.
	ctx, cancelTotal := context.WithTimeoutCause(ctx, r.timeouts.Total, ErrTotalTimeout)
	ctx, cancelStream := context.WithCancelCause(ctx)
	s := &Stream{
		ctx:          ctx,
		cancelTotal:  cancelTotal,
		cancelStream: cancelStream,
		idle:         r.timeouts.Idle,
		model:        t.Model,
	}
	// Until the first content arrives, the watchdog enforces the
	// first-token timeout. It cancels the context instead of racing a
	// goroutine against Next, so a stream needs no goroutine of its own.
	s.watchdog = time.AfterFunc(r.timeouts.FirstToken, func() { cancelStream(ErrFirstTokenTimeout) })

	src, err := t.Provider.ChatStream(ctx, &req)
	if err != nil {
		err = timeoutCause(ctx, err)
		s.Close()
		return nil, res, err
	}
	s.src = src
	if err := s.readToCommit(); err != nil {
		s.Close()
		return nil, res, err
	}
	return s, res, nil
}

// Stream is a committed stream with the gateway's timeouts applied. Next
// and Close are called from one goroutine.
type Stream struct {
	src          provider.Stream
	ctx          context.Context
	cancelTotal  context.CancelFunc
	cancelStream context.CancelCauseFunc
	watchdog     *time.Timer // cancels ctx when the next chunk is late
	idle         time.Duration
	model        string

	pending []*provider.Chunk // read before the commit point, not yet returned
	ended   bool              // the upstream stream is over
}

// readToCommit reads until the commit point and keeps what it read for
// Next. Keepalives before the first content are dropped and, unlike later
// ones, do not extend the deadline: they don't show the model is producing.
func (s *Stream) readToCommit() error {
	for {
		chunk, err := s.read()
		if errors.Is(err, io.EOF) {
			s.ended = true
			break
		}
		if err != nil {
			return err
		}
		if chunk.IsKeepalive() {
			continue
		}
		s.pending = append(s.pending, chunk)
		if chunk.Delta != "" || chunk.FinishReason != "" {
			break
		}
	}
	// Committed: from here on the watchdog enforces the idle timeout. It
	// needs a new timer, because Reset would reschedule the old function,
	// and that one reports a first-token timeout.
	s.watchdog.Stop()
	s.watchdog = time.AfterFunc(s.idle, func() { s.cancelStream(ErrIdleTimeout) })
	return nil
}

// Next returns the next chunk that carries data, or io.EOF at the end. It
// skips keepalives, but every chunk, keepalives included, restarts the
// idle timeout.
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
			return nil, err
		}
		s.watchdog.Reset(s.idle)
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
