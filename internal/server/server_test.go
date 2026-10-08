package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/router"
)

// TestMain fails the package if any test leaves a goroutine running.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fixedTime is the clock reading in tests.
var fixedTime = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// testServer builds a Server with two aliases, "mock-fast" (default 5
// tokens, ceiling 50) and "another", both backed by p. mutate, if not nil,
// adjusts the server config first.
func testServer(t *testing.T, p provider.Provider, mutate func(*config.Server)) *Server {
	t.Helper()
	cfg := config.Server{
		Addr:              "127.0.0.1:0",
		ShutdownGrace:     5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxBodyBytes:      1 << 20,
		MaxInflight:       100,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	target := []config.Target{{Provider: p.Name(), Model: "mock-1"}}
	r, err := router.New(map[string]config.Model{
		"mock-fast": {DefaultMaxTokens: 5, MaxTokensCeiling: 50, Targets: target},
		"another":   {DefaultMaxTokens: 5, MaxTokensCeiling: 50, Targets: target},
	}, map[string]provider.Provider{p.Name(): p})
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, r, slog.New(slog.DiscardHandler))
	s.now = func() time.Time { return fixedTime }
	s.started = fixedTime
	return s
}

// do sends a request through the server's full handler chain, without a
// network. It is safe to call from several goroutines.
func do(ctx context.Context, s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
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
	s := testServer(t, newGate(), nil)

	rec := do(t.Context(), s, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), `{"status":"ok"}`+"\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	if rec := do(t.Context(), s, http.MethodPost, "/healthz", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: status = %d, want 405", rec.Code)
	}
}

func TestServeShutsDownWhenContextIsCancelled(t *testing.T) {
	s := testServer(t, newGate(), nil)
	ln := listen(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	// The server answers over a real connection before shutdown.
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
	s := testServer(t, newGate(), nil)
	ln := listen(t)
	_ = ln.Close() // Accept fails at once on a closed listener.

	if err := s.Serve(t.Context(), ln); err == nil {
		t.Fatal("Serve returned nil on a closed listener, want an error")
	}
}

// gate is a provider whose calls block until the test releases them. Each
// call signals on arrived when it starts.
type gate struct {
	arrived chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{arrived: make(chan struct{}, 1000), release: make(chan struct{})}
}

func (g *gate) Name() string { return "gate" }

func (g *gate) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	g.arrived <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &provider.ChatResponse{
		Model: req.Model, Content: "ok", FinishReason: provider.FinishStop,
		Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1},
	}, nil
}

func (g *gate) ChatStream(context.Context, *provider.ChatRequest) (provider.Stream, error) {
	return nil, errors.New("gate: streaming is not used in these tests")
}
