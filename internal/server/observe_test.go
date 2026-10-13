package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/limiter"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// collectingLog is a RequestLog that keeps every row.
type collectingLog struct {
	mu      sync.Mutex
	entries []usage.Entry
}

func (c *collectingLog) Record(e usage.Entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
	return true
}

func (c *collectingLog) last(t *testing.T) usage.Entry {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		t.Fatal("no request-log row")
	}
	return c.entries[len(c.entries)-1]
}

func TestRequestLogRows(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 4}), nil)
	s.pricing = map[string]config.Price{"mock/mock-1": {InputMicros: 1 * money.PerUSD, OutputMicros: 2 * money.PerUSD}}
	rows := &collectingLog{}
	s.requests = rows

	do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	ok := rows.last(t)
	if ok.RequestID == "" || ok.KeyID != testKey.ID || ok.Alias != "mock-fast" || ok.Provider != "mock" ||
		ok.Model != "mock-1" || ok.Status != 200 || ok.Attempts != 1 || ok.ErrorCode != "" || ok.Stream {
		t.Errorf("success row = %+v", ok)
	}
	// The mock reports 1 prompt token and its 4-word reply: 1×1 + 4×2 micro-USD.
	if ok.PromptTokens != 1 || ok.CompletionTokens != 4 || ok.Cost != 9 || ok.UsageEstimated || ok.Latency <= 0 || ok.TTFT != 0 {
		t.Errorf("success row usage = %+v", ok)
	}

	do(t.Context(), s, http.MethodPost, "/v1/chat/completions",
		`{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`)
	if st := rows.last(t); !st.Stream || st.Status != 200 || st.TTFT <= 0 || st.TTFT > st.Latency || st.CompletionTokens != 4 {
		t.Errorf("stream row = %+v", st)
	}

	do(t.Context(), s, http.MethodPost, "/v1/chat/completions", `{"model": "nope", "messages": [{"role": "user", "content": "hi"}]}`)
	if nf := rows.last(t); nf.Status != 404 || nf.ErrorCode != "model_not_found" || nf.Alias != "nope" || nf.Provider != "" {
		t.Errorf("404 row = %+v", nf)
	}

	doAs(t.Context(), s, "", http.MethodPost, "/v1/chat/completions", chatBody)
	if ua := rows.last(t); ua.Status != 401 || ua.ErrorCode != "invalid_api_key" || ua.KeyID != 0 || ua.Alias != "" {
		t.Errorf("401 row = %+v", ua)
	}

	// Only chat requests get rows.
	before := len(rows.entries)
	do(t.Context(), s, http.MethodGet, "/v1/models", "")
	do(t.Context(), s, http.MethodGet, "/healthz", "")
	if len(rows.entries) != before {
		t.Errorf("%d rows for non-chat requests, want none", len(rows.entries)-before)
	}
}

// A stream that fails after it started is a 200 with an error code.
func TestRequestLogRowForAMidStreamFailure(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 5, FailAtChunk: 3}), nil)
	rows := &collectingLog{}
	s.requests = rows
	do(t.Context(), s, http.MethodPost, "/v1/chat/completions",
		`{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`)
	if e := rows.last(t); e.Status != 200 || e.ErrorCode != "upstream_error" || !e.UsageEstimated || e.CompletionTokens == 0 {
		t.Errorf("row = %+v, want 200 with upstream_error and estimated usage", e)
	}
}

func TestPanicRowIs500(t *testing.T) {
	p := &scripted{chat: func(context.Context) (*provider.ChatResponse, error) { panic("bug") }}
	s := testServer(t, p, nil)
	rows := &collectingLog{}
	s.requests = rows
	do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	if e := rows.last(t); e.Status != 500 || e.ErrorCode != "internal_error" {
		t.Errorf("row = %+v, want 500 internal_error", e)
	}
}

// stalledWriter is a request-log writer that never returns until released,
// like a database that has stopped answering.
type stalledWriter struct{ release chan struct{} }

func (w stalledWriter) WriteRequestLogs(ctx context.Context, _ []usage.Entry) error {
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Acceptance (M6): handlers never block on logging, even with the database
// stalled.
func TestHandlersDoNotWaitForAStalledRequestLog(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	w := stalledWriter{release: make(chan struct{})}
	rec := usage.NewRecorder(w, 5, 2, time.Hour, slog.New(slog.DiscardHandler))
	s.requests = rec
	ctx, cancel := context.WithCancel(context.Background())
	worker := make(chan struct{})
	go func() { rec.Run(ctx); close(worker) }()
	defer func() {
		close(w.release)
		cancel()
		<-worker
	}()

	const n = 50
	done := make(chan int)
	go func() {
		for range n {
			do(context.Background(), s, http.MethodPost, "/v1/chat/completions", chatBody)
		}
		done <- n
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("requests blocked behind the stalled request log")
	}
	// The worker took one batch of 2 and is stuck writing it; the queue
	// holds 5; the rest were dropped and counted.
	if d := rec.Dropped.Load(); d < n-7 {
		t.Errorf("dropped %d rows, want at least %d", d, n-7)
	}
}

func TestMetricsAfterTraffic(t *testing.T) {
	// The first target fails, so the router retries it once and falls back.
	providers := map[string]provider.Provider{
		"down": mock.New("down", mock.Config{Status: 503}),
		"up":   mock.New("up", mock.Config{OutputTokens: 3}),
	}
	s, _ := routedServer(t, providers, []config.Target{{Provider: "down", Model: "m"}, {Provider: "up", Model: "m"}},
		func(c *config.Config) {
			c.Retry.MaxAttemptsPerTarget = 2
			c.Retry.BaseBackoff = time.Millisecond
			c.Retry.MaxBackoff = time.Millisecond
		})
	s.pricing = map[string]config.Price{"up/m": {InputMicros: 1 * money.PerUSD, OutputMicros: 2 * money.PerUSD}}
	s.limiter = limiter.New(func() time.Time { return fixedTime })
	s.keys.(*fakeKeys).keys[testSecret] = auth.Key{ID: 1, RPM: 2, TPM: 1_000_000}

	do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	do(t.Context(), s, http.MethodGet, "/v1/models", "")
	do(t.Context(), s, http.MethodGet, "/v1/models", "") // over 2 a minute
	do(t.Context(), s, http.MethodGet, "/v1/nothing/here/1", "")
	do(t.Context(), s, http.MethodGet, "/v1/nothing/here/2", "")

	text := scrape(t, s)
	for _, want := range []string{
		`tollgate_http_request_duration_seconds_count{route="/v1/chat/completions",status="200"} 1`,
		`tollgate_http_request_duration_seconds_count{route="/v1/models",status="200"} 1`,
		`tollgate_http_request_duration_seconds_count{route="/v1/models",status="429"} 1`,
		// Unknown paths share one label.
		`tollgate_http_request_duration_seconds_count{route="/v1/other",status="429"} 2`,
		`tollgate_upstream_requests_total{outcome="unavailable",provider="down"} 2`,
		`tollgate_upstream_requests_total{outcome="ok",provider="up"} 1`,
		`tollgate_upstream_retries_total{provider="down"} 1`,
		`tollgate_upstream_fallbacks_total{alias="mock-fast"} 1`,
		`tollgate_rejections_total{reason="requests_per_minute"} 3`,
		`tollgate_tokens_total{kind="completion",provider="up"} 3`,
		`tollgate_cost_usd_total{provider="up"} 7e-06`,
		`tollgate_gateway_overhead_seconds_count 1`,
		`tollgate_breaker_state{alias="mock-fast",state="closed",target="down/m"} 1`,
		`tollgate_inflight_requests 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(text, testSecret) || strings.Contains(text, "req_") {
		t.Error("a metric is labelled with a key or request ID")
	}
}

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	AdminHandler(s.metrics).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", rec.Code)
	}
	return rec.Body.String()
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func TestReadyz(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	check := func(want int, wantBody string) {
		t.Helper()
		rec := doAs(t.Context(), s, "", http.MethodGet, "/readyz", "")
		if rec.Code != want || !strings.Contains(rec.Body.String(), wantBody) {
			t.Errorf("/readyz = %d %s, want %d %s", rec.Code, rec.Body, want, wantBody)
		}
	}
	check(http.StatusOK, `"ready"`)
	s.db = pinger{}
	check(http.StatusOK, `"ready"`)
	s.db = pinger{err: errors.New("connection refused")}
	check(http.StatusServiceUnavailable, "database_unavailable")
	s.db = pinger{}
	s.stopping.Store(true)
	check(http.StatusServiceUnavailable, "shutting_down")
}

func TestAdminListener(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- ServeAdmin(ctx, ln, AdminHandler(s.metrics)) }()

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	for path, want := range map[string]string{"/metrics": "tollgate_inflight_requests", "/debug/pprof/": "goroutine"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d, want 200 mentioning %q", path, resp.StatusCode, want)
		}
	}
	client.CloseIdleConnections()
	cancel()
	if err := <-served; err != nil {
		t.Errorf("ServeAdmin: %v", err)
	}
}
