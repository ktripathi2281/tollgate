package auth

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// Lookup finds a key by its hash. It returns ErrUnknownKey if there is no
// such key, and the key with Revoked set if it has been revoked.
type Lookup interface {
	KeyByHash(ctx context.Context, hash [sha256.Size]byte) (Key, error)
}

// Cache authenticates keys, keeping every key it finds, revoked or not, for
// ttl. A key revoked in the database therefore keeps working for up to ttl:
// that is the revocation delay.
//
// Unknown keys aren't cached. Real keys bound the cache's size, and a
// client sending made-up keys can't grow it; each made-up key costs one
// indexed lookup instead. Lookup failures aren't cached either.
type Cache struct {
	lookup Lookup
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[[sha256.Size]byte]cacheEntry
}

type cacheEntry struct {
	key     Key
	expires time.Time
}

// NewCache returns a Cache over lookup. now is the clock; tests pass a fake
// one.
func NewCache(lookup Lookup, ttl time.Duration, now func() time.Time) *Cache {
	return &Cache{lookup: lookup, ttl: ttl, now: now, entries: make(map[[sha256.Size]byte]cacheEntry)}
}

// Authenticate returns the key a request presented. It returns
// ErrUnknownKey or ErrRevokedKey if the key can't be used, or the lookup's
// error if the database couldn't be asked.
func (c *Cache) Authenticate(ctx context.Context, presented string) (Key, error) {
	if !wellFormed(presented) {
		return Key{}, ErrUnknownKey
	}
	hash := Hash(presented)
	now := c.now()

	c.mu.Lock()
	e, ok := c.entries[hash]
	c.mu.Unlock()

	// The lookup runs without the lock held, so a slow database doesn't
	// stall requests for other keys. Two requests that miss at once both
	// look the key up; the second write wins, and both are correct.
	if !ok || !now.Before(e.expires) {
		key, err := c.lookup.KeyByHash(ctx, hash)
		if err != nil {
			return Key{}, err
		}
		e = cacheEntry{key: key, expires: now.Add(c.ttl)}
		c.mu.Lock()
		c.entries[hash] = e
		c.mu.Unlock()
	}

	if e.key.Revoked {
		return Key{}, ErrRevokedKey
	}
	return e.key, nil
}
