package idempotency_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/idempotency"
	"github.com/ktripathi2281/tollgate/internal/store"
	"github.com/ktripathi2281/tollgate/internal/store/storetest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParseKey(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{`"8e03978e-40d5-43e8-bc93-6894a57f9324"`, "8e03978e-40d5-43e8-bc93-6894a57f9324", false},
		{"8e03978e-40d5-43e8-bc93-6894a57f9324", "8e03978e-40d5-43e8-bc93-6894a57f9324", false}, // bare
		{`"with \"quotes\" and \\"`, `with "quotes" and \`, false},
		{`"a b"`, "a b", false},
		{strings.Repeat("k", 255), strings.Repeat("k", 255), false},
		{strings.Repeat("k", 256), "", true},
		{`""`, "", true},
		{`"unterminated`, "", true},
		{`"bad \x escape"`, "", true},
		{`"inner " quote"`, "", true},
		{"tab\there", "", true},
		{"ünïcode", "", true},
	}
	for _, tt := range tests {
		got, err := idempotency.ParseKey(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseKey(%q) = %q, %v; want %q, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const (
	lockFor   = 6 * time.Minute
	retention = 24 * time.Hour
)

// newKeys returns Keys on a fresh schema, the ID of an API key to scope
// idempotency keys to, and the clock.
func newKeys(t *testing.T, maxResponse int) (*idempotency.Keys, int64, *clock) {
	t.Helper()
	k, s, clk := newKeysWithStore(t, maxResponse)
	return k, createAPIKey(t, s), clk
}

func newKeysWithStore(t *testing.T, maxResponse int) (*idempotency.Keys, *store.Store, *clock) {
	t.Helper()
	s := storetest.Open(t)
	clk := &clock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	return idempotency.New(s, lockFor, retention, maxResponse, clk.Now, slog.New(slog.DiscardHandler)), s, clk
}

func createAPIKey(t *testing.T, s *store.Store) int64 {
	t.Helper()
	secret := auth.Generate()
	id, _, err := s.CreateKey(t.Context(), store.NewKey{Name: "k", Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret), RPM: 1, TPM: 1})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func begin(t *testing.T, k *idempotency.Keys, keyID int64, idemKey, body, requestID string) idempotency.Result {
	t.Helper()
	res, err := k.Begin(t.Context(), keyID, idemKey, []byte(body), requestID)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return res
}

func TestReplayAfterCompletion(t *testing.T) {
	k, keyID, _ := newKeys(t, 1<<20)
	first := begin(t, k, keyID, "k1", `{"a":1}`, "req_1")
	if first.Outcome != idempotency.Proceed {
		t.Fatalf("first use: outcome %v, want Proceed", first.Outcome)
	}
	if err := k.Complete(t.Context(), first.Lock, 200, []byte(`{"id":"done"}`)); err != nil {
		t.Fatal(err)
	}
	again := begin(t, k, keyID, "k1", `{"a":1}`, "req_2")
	if again.Outcome != idempotency.Replay || again.Status != 200 || string(again.Body) != `{"id":"done"}` {
		t.Errorf("retry = %+v, want a replay of the stored response", again)
	}
}

func TestDifferentBodyIsAMismatch(t *testing.T) {
	k, keyID, _ := newKeys(t, 1<<20)
	first := begin(t, k, keyID, "k1", `{"a":1}`, "req_1")
	// Mismatch is reported while the first is running, and after it completes.
	if res := begin(t, k, keyID, "k1", `{"a": 1}`, "req_2"); res.Outcome != idempotency.Mismatch {
		t.Errorf("same JSON, different bytes, while running: outcome %v, want Mismatch", res.Outcome)
	}
	if err := k.Complete(t.Context(), first.Lock, 200, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if res := begin(t, k, keyID, "k1", `{"a":2}`, "req_3"); res.Outcome != idempotency.Mismatch {
		t.Errorf("different body after completion: outcome %v, want Mismatch", res.Outcome)
	}
}

// The same idempotency key under two API keys is two separate records.
func TestKeysAreScopedToTheAPIKey(t *testing.T) {
	k, s, _ := newKeysWithStore(t, 1<<20)
	alice, bob := createAPIKey(t, s), createAPIKey(t, s)
	begin(t, k, alice, "shared", `{"from":"alice"}`, "req_1")
	if res := begin(t, k, bob, "shared", `{"from":"bob"}`, "req_2"); res.Outcome != idempotency.Proceed {
		t.Errorf("another API key with the same idempotency key: outcome %v, want Proceed", res.Outcome)
	}
}

func TestInProgressThenAbandonedAndRetried(t *testing.T) {
	k, keyID, _ := newKeys(t, 1<<20)
	first := begin(t, k, keyID, "k1", `{}`, "req_1")
	res := begin(t, k, keyID, "k1", `{}`, "req_2")
	if res.Outcome != idempotency.InProgress || res.RetryAfter <= 0 {
		t.Fatalf("while running: %+v, want InProgress with a Retry-After", res)
	}
	// The first request fails: its record goes, so a retry runs.
	if err := k.Abandon(t.Context(), first.Lock); err != nil {
		t.Fatal(err)
	}
	if res := begin(t, k, keyID, "k1", `{}`, "req_3"); res.Outcome != idempotency.Proceed {
		t.Errorf("after the first failed: outcome %v, want Proceed", res.Outcome)
	}
}

func TestTakeOverAfterTheLockExpires(t *testing.T) {
	k, keyID, clk := newKeys(t, 1<<20)
	crashed := begin(t, k, keyID, "k1", `{}`, "req_1") // never completes

	clk.advance(lockFor - time.Second)
	if res := begin(t, k, keyID, "k1", `{}`, "req_2"); res.Outcome != idempotency.InProgress {
		t.Fatalf("before the lock expires: outcome %v, want InProgress", res.Outcome)
	}
	clk.advance(time.Second)
	if res := begin(t, k, keyID, "k1", `{"other":1}`, "req_3"); res.Outcome != idempotency.Mismatch {
		t.Errorf("a different body can't take over: outcome %v, want Mismatch", res.Outcome)
	}
	takeover := begin(t, k, keyID, "k1", `{}`, "req_4")
	if takeover.Outcome != idempotency.Proceed {
		t.Fatalf("after the lock expires: outcome %v, want Proceed", takeover.Outcome)
	}

	// The crashed request comes back and completes: it no longer owns the
	// record, so its response isn't stored.
	if err := k.Complete(t.Context(), crashed.Lock, 200, []byte(`"stale"`)); err != nil {
		t.Fatal(err)
	}
	if err := k.Abandon(t.Context(), crashed.Lock); err != nil {
		t.Fatal(err)
	}
	if res := begin(t, k, keyID, "k1", `{}`, "req_5"); res.Outcome != idempotency.InProgress {
		t.Fatalf("the takeover's record was disturbed: outcome %v, want InProgress", res.Outcome)
	}
	if err := k.Complete(t.Context(), takeover.Lock, 200, []byte(`"fresh"`)); err != nil {
		t.Fatal(err)
	}
	if res := begin(t, k, keyID, "k1", `{}`, "req_6"); res.Outcome != idempotency.Replay || string(res.Body) != `"fresh"` {
		t.Errorf("replay = %+v, want the takeover's response", res)
	}
}

func TestExpiredRecordsAreReplacedAndSwept(t *testing.T) {
	k, keyID, clk := newKeys(t, 1<<20)
	first := begin(t, k, keyID, "k1", `{}`, "req_1")
	if err := k.Complete(t.Context(), first.Lock, 200, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	begin(t, k, keyID, "k2", `{}`, "req_2")

	clk.advance(retention)
	// Past its retention, a key works as new, even with a different body.
	if res := begin(t, k, keyID, "k1", `{"new":1}`, "req_3"); res.Outcome != idempotency.Proceed {
		t.Errorf("an expired key: outcome %v, want Proceed", res.Outcome)
	}
	if n, err := k.Sweep(t.Context()); err != nil || n != 1 {
		t.Errorf("Sweep = %d, %v; want k2's record deleted", n, err)
	}
}

func TestResponsesOverTheCapAreNotStored(t *testing.T) {
	k, keyID, _ := newKeys(t, 10)
	first := begin(t, k, keyID, "k1", `{}`, "req_1")
	if err := k.Complete(t.Context(), first.Lock, 200, []byte("more than ten bytes")); err != nil {
		t.Fatal(err)
	}
	if res := begin(t, k, keyID, "k1", `{}`, "req_2"); res.Outcome != idempotency.NotStored {
		t.Errorf("outcome %v, want NotStored: the request ran, and must not run again", res.Outcome)
	}
}

// Of N concurrent requests with one key, exactly one proceeds.
func TestConcurrentClaims(t *testing.T) {
	k, keyID, _ := newKeys(t, 1<<20)
	var proceeded, inProgress atomic.Int64
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			res, err := k.Begin(context.Background(), keyID, "k1", []byte("{}"), fmt.Sprintf("req_%d", i))
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			switch res.Outcome {
			case idempotency.Proceed:
				proceeded.Add(1)
			case idempotency.InProgress:
				inProgress.Add(1)
			}
		})
	}
	wg.Wait()
	if proceeded.Load() != 1 || inProgress.Load() != 49 {
		t.Errorf("%d proceeded and %d were told it's in progress; want 1 and 49", proceeded.Load(), inProgress.Load())
	}
}
