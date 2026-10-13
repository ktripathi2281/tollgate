package main

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/store/storetest"
)

// get fetches a URL with a client of its own, and returns the status and
// body.
func get(t *testing.T, url, key string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// End to end: /readyz on the public listener, metrics on the admin one,
// and every chat request's row in the request log, written by the time
// the gateway has shut down.
func TestServeObservability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't send SIGTERM to another process")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	dbURL := storetest.URL(t)
	secret := createKey(t, dbURL)
	// A long flush interval: the rows can only reach the table through the
	// drain at shutdown.
	yaml := mockConfig + "server:\n  addr: \"127.0.0.1:0\"\n  admin_addr: \"127.0.0.1:0\"\n  shutdown_grace: 5s\n" +
		"request_log: { flush_every: 1h }\n"
	c := startServe(t, ctx, yaml, dbURL)
	if c.adminAddr == "" {
		t.Fatal("the admin listener didn't start")
	}

	if status, body := get(t, "http://"+c.addr+"/readyz", ""); status != http.StatusOK || !strings.Contains(body, "ready") {
		t.Errorf("/readyz = %d %s, want 200 ready", status, body)
	}
	client := &http.Client{Transport: &http.Transport{}}
	for _, body := range []string{
		`{"model": "mock-fast", "messages": [{"role": "user", "content": "hi"}]}`,
		`{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`,
		`{"model": "no-such-alias", "messages": [{"role": "user", "content": "hi"}]}`,
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.addr+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	client.CloseIdleConnections()

	// Metrics are on the admin listener only.
	if status, _ := get(t, "http://"+c.addr+"/metrics", secret); status == http.StatusOK {
		t.Error("/metrics answered on the public listener")
	}
	status, metrics := get(t, "http://"+c.adminAddr+"/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("/metrics on the admin listener: %d", status)
	}
	for _, want := range []string{
		`tollgate_http_request_duration_seconds_count{route="/v1/chat/completions",status="200"} 2`,
		`tollgate_http_request_duration_seconds_count{route="/v1/chat/completions",status="404"} 1`,
		`tollgate_upstream_requests_total{outcome="ok",provider="mock"} 2`,
		`tollgate_tokens_total{kind="completion",provider="mock"} 10`,
		`tollgate_time_to_first_token_seconds_count{provider="mock"} 1`,
		`tollgate_breaker_state{alias="mock-fast",state="closed",target="mock/mock-1"} 1`,
		`tollgate_inflight_requests 0`,
		`tollgate_request_logs_written_total 0`, // a batch isn't full and the interval is an hour
		`tollgate_budget_settle_failures_total 0`,
		`go_goroutines`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if status, _ := get(t, "http://"+c.adminAddr+"/debug/pprof/", ""); status != http.StatusOK {
		t.Errorf("pprof index: %d", status)
	}

	if _, err := c.terminate(t); err != nil {
		t.Fatalf("child exited with %v\nstderr:\n%s", err, c.stderr)
	}
	// Acceptance (M6), end to end: the worker drains on shutdown.
	if n := storetest.Int(t, dbURL, "SELECT count(*) FROM request_logs"); n != 3 {
		t.Errorf("%d request-log rows after shutdown, want 3", n)
	}
	if n := storetest.Int(t, dbURL, `SELECT count(*) FROM request_logs
		WHERE alias = 'no-such-alias' AND status = 404 AND error_code = 'model_not_found' AND provider IS NULL`); n != 1 {
		t.Error("the 404's row doesn't match")
	}
	if n := storetest.Int(t, dbURL, `SELECT count(*) FROM request_logs
		WHERE stream AND status = 200 AND provider = 'mock' AND completion_tokens = 5 AND ttft_us > 0`); n != 1 {
		t.Error("the stream's row doesn't match")
	}
}
