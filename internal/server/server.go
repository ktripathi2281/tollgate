// Package server runs the gateway's public HTTP server: routing, middleware
// and graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/ktripathi2281/tollgate/internal/config"
)

// Server is the public HTTP server.
type Server struct {
	cfg  config.Server
	log  *slog.Logger
	http *http.Server
}

// New builds a Server and its routes. It doesn't listen yet; call Serve.
func New(cfg config.Server, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)

	return &Server{
		cfg: cfg,
		log: logger,
		http: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			// There is deliberately no WriteTimeout: it would cut off long
			// streaming responses. Streams get per-request deadlines instead.
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
	}
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

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client has gone; there is nobody to tell.
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}
