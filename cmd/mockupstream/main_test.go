package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestRunServesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-ttft", "0s", "-token-interval", "0s", "-output-tokens", "2"}, pw)
		_ = pw.Close()
	}()

	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the listen address: %v", err)
	}
	addr := strings.TrimSpace(strings.TrimPrefix(line, "listening on "))
	go func() { _, _ = io.Copy(io.Discard, pr) }() // drain any later output

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/v1/chat/completions",
		strings.NewReader(`{"model": "mock-1", "messages": [{"role": "user", "content": "hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("run returned %v after cancellation, want nil", err)
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-output-tokens", "0"},
		{"-error-rate", "2"},
		{"-status", "200"},
		{"-ttft", "-1s"},
		{"-fail-at-chunk", "-1"},
		{"-unknown"},
	} {
		if err := run(t.Context(), args, io.Discard); err == nil {
			t.Errorf("run(%q) = nil, want an error", args)
		}
	}
}
