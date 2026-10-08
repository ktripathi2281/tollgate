// Package server runs the gateway's public HTTP server: routing, middleware,
// handlers and graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/router"
)

// unwindTimeout bounds how long requests cancelled at the end of the
// shutdown grace period get to finish (send a final error event, release
// what they hold) before their connections are closed.
const unwindTimeout = 5 * time.Second

// Server is the public HTTP server.
type Server struct {
	cfg      config.Server
	timeouts config.Timeouts
	log      *slog.Logger
	router   *router.Router
	now      func() time.Time // the clock for response timestamps; tests replace it
	started  time.Time
	http     *http.Server
	handlers sync.WaitGroup // requests being handled, so shutdown can wait for them
}

// New builds a Server and its routes. It doesn't listen yet; call Serve.
func New(cfg config.Server, timeouts config.Timeouts, r *router.Router, logger *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		timeouts: timeouts,
		log:      logger,
		router:   r,
		now:      time.Now,
		started:  time.Now(),
	}
	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		// There is deliberately no WriteTimeout: it would cut off long
		// streams. Streams get per-request deadlines instead.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s
}

// routes builds the handler tree. Middleware runs outside in: shutdown
// tracking, a request ID, the access log, then panic recovery. API requests
// then pass the in-flight cap; health checks skip it so they still answer
// when the gateway is saturated.
func (s *Server) routes() http.Handler {
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	apiMux.HandleFunc("GET /v1/models", s.handleModels)
	apiMux.HandleFunc("/v1/", handleUnknown)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("/v1/", limitInflight(s.cfg.MaxInflight, apiMux))

	return s.trackHandlers(withRequestID(logRequests(s.log, recoverPanics(s.log, mux))))
}

// trackHandlers counts running handlers, so shutdown can wait for them.
func (s *Server) trackHandlers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlers.Add(1)
		defer s.handlers.Done()
		next.ServeHTTP(w, r)
	})
}

// Serve accepts connections on ln until ctx is cancelled, then shuts down:
//
//  1. Stop accepting, and let in-flight requests, streams included, finish
//     for up to the shutdown grace period.
//  2. If any are still running, cancel their contexts with errShuttingDown,
//     so streams end with an error event instead of being cut off, and wait
//     a short, bounded time for them to return.
//  3. Close whatever connections are left.
//
// It returns nil if every request finished within the grace period, and an
// error if the server failed or requests had to be cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Every request context derives from baseCtx, so cancelling it reaches
	// every handler still running.
	baseCtx, cancelBase := context.WithCancelCause(context.Background())
	defer cancelBase(nil)
	s.http.BaseContext = func(net.Listener) context.Context { return baseCtx }

	// http.Server.Serve blocks, so it runs in its own goroutine and reports
	// how it ended on errc. The buffer of one lets that goroutine exit even
	// if nothing is receiving.
	errc := make(chan error, 1)
	go func() {
		errc <- s.http.Serve(ln)
	}()

	select {
	case err := <-errc:
		// Serve only returns early if the listener fails.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "grace", s.cfg.ShutdownGrace.String())

	// ctx is already cancelled, so the grace period needs a fresh context.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownGrace)
	defer cancel()

	shutdownErr := s.http.Shutdown(shutdownCtx)
	if errors.Is(shutdownErr, context.DeadlineExceeded) {
		s.log.Warn("grace period over, cancelling remaining requests")
		cancelBase(errShuttingDown)
		if !s.waitForHandlers(unwindTimeout) {
			s.log.Warn("requests still running after cancellation, closing their connections")
		}
		if err := s.http.Close(); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}

	// Once Shutdown or Close has started, Serve returns ErrServerClosed.
	// Waiting for it means the goroutine above has exited.
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	return nil
}

// waitForHandlers waits up to d for every running handler to return. It
// reports whether they all did. If they didn't, the goroutine waiting on
// the WaitGroup stays until they do; the process is exiting anyway.
func (s *Server) waitForHandlers(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
