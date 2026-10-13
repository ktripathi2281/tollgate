package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/store"
	"github.com/ktripathi2281/tollgate/internal/store/storetest"
)

// Acceptance (M5): tollgate reconcile passes on consistent books, and
// fails when a row is tampered with.
func TestReconcile(t *testing.T) {
	s, dbURL := storetest.OpenWithURL(t)
	t.Setenv(databaseURLEnv, dbURL)
	ctx := t.Context()

	secret := auth.Generate()
	keyID, _, err := s.CreateKey(ctx, store.NewKey{
		Name: "budgeted", Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret),
		RPM: 1, TPM: 1, Budget: 1000, BudgetPeriod: "daily",
	})
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	now := day.Add(time.Hour)
	hold := func(amount int64) int64 {
		id, err := s.PlaceHold(ctx, store.HoldParams{
			KeyID: keyID, PeriodStart: day, RequestID: "req", Amount: 300, Budget: 1000, Now: now, ExpiresAt: now.Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	settled, released := hold(300), hold(300)
	hold(300) // still running
	if _, err := s.SettleHold(ctx, settled, 120, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseHold(ctx, released, now); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"reconcile"}, &stdout, &stderr); err != nil {
		t.Fatalf("reconcile on consistent books: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "checked 1 budget periods: 0 mismatched") {
		t.Errorf("output = %q", stdout.String())
	}

	// Someone edits the totals by hand.
	storetest.Int(t, dbURL, "UPDATE budget_periods SET spent_micros = spent_micros + 1 RETURNING 1")
	stdout.Reset()
	err = run(ctx, []string{"reconcile"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "1 budget periods don't match") {
		t.Fatalf("reconcile after tampering: got %v, want an error", err)
	}
	for _, want := range []string{"MISMATCH key", "spent $0.000121, holds settled $0.00012", "held $0.0003, holds held $0.0003"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stdout.String())
		}
	}
}

// End to end: the real binary spends a key's budget, refuses the request
// that doesn't fit, and leaves books that reconcile.
func TestServeSpendsABudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't send SIGTERM to another process")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, dbURL := storetest.OpenWithURL(t)
	secret := auth.Generate()
	// A request for "hi" holds (16+16+2) × $1/M + 64 × $2/M = 162
	// micro-USD, and then spends 11: 1 prompt token at $1/M and the mock's
	// 5-token reply at $2/M. With a budget of 165, the first hold fits; the
	// second needs 11 + 162 = 173, which doesn't.
	if _, _, err := s.CreateKey(ctx, store.NewKey{
		Name: "budgeted", Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret),
		RPM: 100, TPM: 100_000, Budget: 165, BudgetPeriod: "daily",
	}); err != nil {
		t.Fatal(err)
	}
	c := startServe(t, ctx, mockConfig+"server:\n  addr: \"127.0.0.1:0\"\n  admin_addr: \"127.0.0.1:0\"\n  shutdown_grace: 5s\n", dbURL)

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	post := func() int {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.addr+"/v1/chat/completions",
			strings.NewReader(`{"model": "mock-fast", "messages": [{"role": "user", "content": "hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("first request: %d, want 200", code)
	}
	if code := post(); code != http.StatusPaymentRequired {
		t.Errorf("second request: %d, want 402", code)
	}
	// Close the client's connections first. The server counts an open
	// connection as busy, and shutdown would wait for it.
	client.CloseIdleConnections()
	if _, err := c.terminate(t); err != nil {
		t.Errorf("child exited with %v\nstderr:\n%s", err, c.stderr)
	}

	t.Setenv(databaseURLEnv, dbURL)
	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"reconcile"}, &stdout, io.Discard); err != nil {
		t.Errorf("reconcile: %v\n%s", err, stdout.String())
	}
	storetest.CheckBudgets(t, s, dbURL)
}

func TestSweepEvery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var sweeps atomic.Int64
		done := make(chan struct{})
		go func() {
			sweepEvery(ctx, time.Minute, func(context.Context) { sweeps.Add(1) })
			close(done)
		}()

		synctest.Wait()
		if n := sweeps.Load(); n != 1 {
			t.Fatalf("%d sweeps at start, want 1", n)
		}
		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		if n := sweeps.Load(); n != 4 {
			t.Fatalf("%d sweeps after 3 minutes, want 4", n)
		}
		cancel()
		<-done
	})
}
