package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/config"
)

// TestMain fails the package if any test leaves a goroutine running.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func testConfig() config.Server {
	return config.Server{
		Addr:              "127.0.0.1:0",
		ShutdownGrace:     5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func TestHealthz(t *testing.T) {
	s := New(testConfig(), slog.New(slog.DiscardHandler))

	tests := []struct {
		method     string
		wantStatus int
		wantBody   string
	}{
		{http.MethodGet, http.StatusOK, `{"status":"ok"}` + "\n"},
		{http.MethodPost, http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tt.method, "/healthz", nil)
			rec := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody == "" {
				return
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestServeShutsDownWhenContextIsCancelled(t *testing.T) {
	s := New(testConfig(), slog.New(slog.DiscardHandler))
	ln := listen(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	// The server answers before shutdown.
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v after a clean shutdown, want nil", err)
	}
}

func TestServeReturnsListenerErrors(t *testing.T) {
	s := New(testConfig(), slog.New(slog.DiscardHandler))
	ln := listen(t)
	_ = ln.Close() // Accept fails at once on a closed listener.

	if err := s.Serve(t.Context(), ln); err == nil {
		t.Fatal("Serve returned nil on a closed listener, want an error")
	}
}
