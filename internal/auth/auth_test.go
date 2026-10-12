package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerate(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		k := Generate()
		if !wellFormed(k) {
			t.Fatalf("Generate() = %q, which is not well formed", k)
		}
		if len(k) != 55 || !strings.HasPrefix(k, "tg_") {
			t.Fatalf("Generate() = %q, want tg_ and 52 characters", k)
		}
		if seen[k] {
			t.Fatalf("Generate() repeated %q", k)
		}
		seen[k] = true
	}
}

func TestWellFormed(t *testing.T) {
	good := Generate()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"generated", good, true},
		{"empty", "", false},
		{"no prefix", "xx_" + good[3:], false},
		{"too short", good[:len(good)-1], false},
		{"too long", good + "a", false},
		{"uppercase", "tg_" + strings.ToUpper(good[3:]), false},
		{"not base32", good[:len(good)-1] + "1", false},
		{"an OpenAI-style key", "sk-proj-abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wellFormed(tt.key); got != tt.want {
				t.Errorf("wellFormed(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestHashAndPrefix(t *testing.T) {
	k := Generate()
	if Hash(k) != sha256.Sum256([]byte(k)) {
		t.Error("Hash is not SHA-256 of the key")
	}
	if Hash(k) == Hash(Generate()) {
		t.Error("two keys hash the same")
	}
	if p := DisplayPrefix(k); p != k[:11] {
		t.Errorf("DisplayPrefix = %q, want %q", p, k[:11])
	}
	if p := DisplayPrefix("tg_"); p != "tg_" {
		t.Errorf("DisplayPrefix of a short string = %q, want it unchanged", p)
	}
}

func TestAllows(t *testing.T) {
	all := Key{}
	some := Key{AllowedModels: []string{"fast", "mock-fast"}}
	if !all.Allows("anything") {
		t.Error("a key without a list must allow every alias")
	}
	if !some.Allows("mock-fast") || some.Allows("slow") {
		t.Error("a key with a list must allow exactly the aliases on it")
	}
}

// fakeLookup is a Lookup backed by a map, counting calls.
type fakeLookup struct {
	mu    sync.Mutex
	keys  map[[sha256.Size]byte]Key
	err   error // returned by every call, if set
	calls int
}

func (f *fakeLookup) KeyByHash(_ context.Context, hash [sha256.Size]byte) (Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Key{}, f.err
	}
	k, ok := f.keys[hash]
	if !ok {
		return Key{}, ErrUnknownKey
	}
	return k, nil
}

func (f *fakeLookup) set(secret string, k Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[Hash(secret)] = k
}

func (f *fakeLookup) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// clock is a fake clock that only moves when told to.
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

func newTestCache() (*Cache, *fakeLookup, *clock, string) {
	lookup := &fakeLookup{keys: make(map[[sha256.Size]byte]Key)}
	clk := &clock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	secret := Generate()
	lookup.set(secret, Key{ID: 7, Name: "test", RPM: 60, TPM: 1000})
	return NewCache(lookup, 30*time.Second, clk.Now), lookup, clk, secret
}

func TestCacheAuthenticates(t *testing.T) {
	c, lookup, _, secret := newTestCache()
	for range 3 {
		k, err := c.Authenticate(t.Context(), secret)
		if err != nil || k.ID != 7 {
			t.Fatalf("Authenticate = %+v, %v; want key 7", k, err)
		}
	}
	if n := lookup.callCount(); n != 1 {
		t.Errorf("looked up %d times, want 1: later calls should hit the cache", n)
	}
}

// Acceptance (M4): a revoked key stops working within the cache TTL.
func TestCacheRevocationTakesEffectWithinTTL(t *testing.T) {
	c, lookup, clk, secret := newTestCache()
	if _, err := c.Authenticate(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	lookup.set(secret, Key{ID: 7, Revoked: true})

	clk.advance(29 * time.Second)
	if _, err := c.Authenticate(t.Context(), secret); err != nil {
		t.Errorf("within the TTL the cached key still works; got %v", err)
	}
	clk.advance(time.Second)
	if _, err := c.Authenticate(t.Context(), secret); !errors.Is(err, ErrRevokedKey) {
		t.Errorf("after the TTL: got %v, want ErrRevokedKey", err)
	}
	// The revocation is cached too.
	if _, err := c.Authenticate(t.Context(), secret); !errors.Is(err, ErrRevokedKey) {
		t.Errorf("cached revocation: got %v, want ErrRevokedKey", err)
	}
	if n := lookup.callCount(); n != 2 {
		t.Errorf("looked up %d times, want 2", n)
	}
}

func TestCacheRejectsMalformedKeysWithoutALookup(t *testing.T) {
	c, lookup, _, _ := newTestCache()
	for _, k := range []string{"", "tg_short", "Bearer tg_x", "sk-abc"} {
		if _, err := c.Authenticate(t.Context(), k); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("Authenticate(%q) = %v, want ErrUnknownKey", k, err)
		}
	}
	if n := lookup.callCount(); n != 0 {
		t.Errorf("looked up %d times, want 0", n)
	}
}

func TestCacheDoesNotCacheUnknownKeysOrFailures(t *testing.T) {
	c, lookup, _, _ := newTestCache()
	unknown := Generate()
	for range 2 {
		if _, err := c.Authenticate(t.Context(), unknown); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("got %v, want ErrUnknownKey", err)
		}
	}
	if n := lookup.callCount(); n != 2 {
		t.Errorf("unknown key looked up %d times, want 2 (not cached)", n)
	}

	// A key created after a failed lookup works at once.
	lookup.set(unknown, Key{ID: 8})
	if k, err := c.Authenticate(t.Context(), unknown); err != nil || k.ID != 8 {
		t.Errorf("new key: got %+v, %v", k, err)
	}

	// A database failure is returned, and the next call tries again.
	down := errors.New("database is down")
	lookup.mu.Lock()
	lookup.err = down
	lookup.mu.Unlock()
	other := Generate()
	for range 2 {
		if _, err := c.Authenticate(t.Context(), other); !errors.Is(err, down) {
			t.Fatalf("got %v, want the lookup's error", err)
		}
	}
	if n := lookup.callCount(); n != 5 {
		t.Errorf("looked up %d times in all, want 5", n)
	}
}

func TestCacheConcurrentUse(t *testing.T) {
	c, _, clk, secret := newTestCache()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			if i%10 == 0 {
				clk.advance(time.Second)
			}
			if _, err := c.Authenticate(context.Background(), secret); err != nil {
				t.Errorf("Authenticate: %v", err)
			}
		})
	}
	wg.Wait()
}
