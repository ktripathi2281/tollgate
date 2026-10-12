// Package auth handles virtual API keys: their format, the hash the
// database stores, and a cache that keeps most requests away from the
// database.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"slices"
	"strings"
)

// Prefix starts every key, so a leaked one is easy to recognise, for
// example by secret scanners.
const Prefix = "tg_"

// secretBytes is how much randomness a key carries: 256 bits.
const secretBytes = 32

// encoding writes the random part in lowercase base32, which has no
// characters that need escaping and none that look alike.
var encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// keyLen is the length of every key: the prefix plus 52 characters.
var keyLen = len(Prefix) + encoding.EncodedLen(secretBytes)

// displayLen is how much of a key is kept in the clear to tell keys apart:
// the prefix and 8 characters, 40 bits of the secret, too little to help
// guess the rest.
const displayLen = len(Prefix) + 8

// Errors from Authenticate. Both mean the request is unauthenticated.
var (
	ErrUnknownKey = errors.New("auth: unknown API key")
	ErrRevokedKey = errors.New("auth: the API key has been revoked")
)

// Key is what the gateway knows about a virtual key: never the secret
// itself, only its settings.
type Key struct {
	ID            int64
	Name          string
	Prefix        string   // the start of the key, for display
	RPM           int      // requests per minute
	TPM           int      // tokens per minute
	AllowedModels []string // the aliases it may use; nil allows every alias
	Revoked       bool
}

// Allows reports whether the key may use the alias.
func (k Key) Allows(alias string) bool {
	return k.AllowedModels == nil || slices.Contains(k.AllowedModels, alias)
}

// Generate returns a new random key. It is shown once, at creation, and
// only its hash is stored.
func Generate() string {
	b := make([]byte, secretBytes)
	// crypto/rand.Read never returns an error; it crashes the program if
	// the system's random source fails, rather than return weak bytes.
	_, _ = rand.Read(b)
	return Prefix + encoding.EncodeToString(b)
}

// Hash returns the SHA-256 hash the database stores for a key. A fast hash
// is enough: a key has 256 random bits, so unlike a password it can't be
// found by trying likely values, and every request has to hash one.
func Hash(key string) [sha256.Size]byte {
	return sha256.Sum256([]byte(key))
}

// DisplayPrefix returns the start of a key, the part shown in listings.
func DisplayPrefix(key string) string {
	return key[:min(displayLen, len(key))]
}

// wellFormed reports whether s looks like a key the gateway issued. It lets
// junk be rejected without a database lookup.
func wellFormed(s string) bool {
	if len(s) != keyLen || !strings.HasPrefix(s, Prefix) {
		return false
	}
	_, err := encoding.DecodeString(s[len(Prefix):])
	return err == nil
}
