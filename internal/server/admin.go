package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/ktripathi2281/tollgate/internal/metrics"
)

// readyTimeout bounds the database check in /readyz.
const readyTimeout = 2 * time.Second

// handleReadyz serves GET /readyz: 200 when the gateway can take traffic,
// 503 while it is shutting down or the database doesn't answer. Unlike
// /healthz, which only says the process is alive, a load balancer uses
// this to decide where to send requests.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	status, body := http.StatusOK, `{"status":"ready"}`
	switch {
	case s.stopping.Load():
		status, body = http.StatusServiceUnavailable, `{"status":"shutting_down"}`
	case s.db != nil:
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			s.log.WarnContext(r.Context(), "not ready: the database doesn't answer", "error", err)
			status, body = http.StatusServiceUnavailable, `{"status":"database_unavailable"}`
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}

// AdminHandler serves the admin listener: Prometheus metrics at /metrics,
// and Go's profiler under /debug/pprof/. Neither belongs on the public
// listener, which is why the admin one binds to localhost by default.
func AdminHandler(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// adminShutdown bounds how long a request on the admin listener, such as a
// CPU profile, may delay shutdown.
const adminShutdown = 5 * time.Second

// ServeAdmin serves h on ln until ctx is done, then shuts down, giving
// requests in progress a few seconds before closing their connections.
func ServeAdmin(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("admin listener: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), adminShutdown)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("admin listener: %w", err)
	}
	return nil
}
