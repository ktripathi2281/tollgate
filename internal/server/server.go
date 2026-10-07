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
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/router"
)

// Server is the public HTTP server.
type Server struct {
	cfg     config.Server
	log     *slog.Logger
	router  *router.Router
	now     func() time.Time // the clock; tests replace it
	started time.Time
	http    *http.Server
}

// New builds a Server and its routes. It doesn't listen yet; call Serve.
func New(cfg config.Server, r *router.Router, logger *slog.Logger) *Server {
	s := &Server{
		cfg:     cfg,
		log:     logger,
		router:  r,
		now:     time.Now,
		started: time.Now(),
	}
	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		// There is deliberately no WriteTimeout: it would cut off long
		// streaming responses. Streams get per-request deadlines instead.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s
}

// routes builds the handler tree. Middleware runs outside in: every request
// gets an ID first, then the access log, then panic recovery. API requests
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

	return withRequestID(logRequests(s.log, recoverPanics(s.log, mux)))
}

// Serve accepts connections on ln until ctx is cancelled, then shuts down:
// it stops accepting, waits up to the shutdown grace period for in-flight
// requests to finish, and closes whatever is still open after that.
//
// It returns nil after a clean shutdown and an error if the server fails or
// the grace period runs out.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
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
		s.log.Warn("grace period over, closing remaining connections")
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
