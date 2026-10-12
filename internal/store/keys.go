package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/store/db"
)

// ErrNotFound is returned when a row asked for by ID doesn't exist.
var ErrNotFound = errors.New("store: not found")

// NewKey is a key to create. Only the hash of its secret is stored.
type NewKey struct {
	Name          string
	Hash          [sha256.Size]byte
	Prefix        string
	RPM           int
	TPM           int
	AllowedModels []string // nil allows every alias; it must not be empty
}

// KeyInfo is a key as listed.
type KeyInfo struct {
	auth.Key
	CreatedAt time.Time
	RevokedAt *time.Time
}

// CreateKey stores a new key and returns its ID and creation time.
func (s *Store) CreateKey(ctx context.Context, k NewKey) (int64, time.Time, error) {
	// The columns are 32-bit; check before converting rather than wrap.
	if k.RPM > math.MaxInt32 || k.TPM > math.MaxInt32 {
		return 0, time.Time{}, fmt.Errorf("create key: rpm and tpm must be at most %d", math.MaxInt32)
	}
	row, err := s.q.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		Name:          k.Name,
		KeyHash:       k.Hash[:],
		KeyPrefix:     k.Prefix,
		Rpm:           int32(k.RPM),
		Tpm:           int32(k.TPM),
		AllowedModels: k.AllowedModels,
	})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("create key: %w", err)
	}
	return row.ID, row.CreatedAt, nil
}

// KeyByHash returns the key with the given hash, revoked or not, or
// auth.ErrUnknownKey if there is none. It implements auth.Lookup.
func (s *Store) KeyByHash(ctx context.Context, hash [sha256.Size]byte) (auth.Key, error) {
	row, err := s.q.GetAPIKeyByHash(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Key{}, auth.ErrUnknownKey
	}
	if err != nil {
		return auth.Key{}, fmt.Errorf("look up key: %w", err)
	}
	return auth.Key{
		ID:            row.ID,
		Name:          row.Name,
		Prefix:        row.KeyPrefix,
		RPM:           int(row.Rpm),
		TPM:           int(row.Tpm),
		AllowedModels: row.AllowedModels,
		Revoked:       row.RevokedAt != nil,
	}, nil
}

// ListKeys returns every key, oldest first.
func (s *Store) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := s.q.ListAPIKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	keys := make([]KeyInfo, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, KeyInfo{
			Key: auth.Key{
				ID:            row.ID,
				Name:          row.Name,
				Prefix:        row.KeyPrefix,
				RPM:           int(row.Rpm),
				TPM:           int(row.Tpm),
				AllowedModels: row.AllowedModels,
				Revoked:       row.RevokedAt != nil,
			},
			CreatedAt: row.CreatedAt,
			RevokedAt: row.RevokedAt,
		})
	}
	return keys, nil
}

// RevokeKey revokes a key and returns when it was revoked. Revoking a
// revoked key changes nothing and returns the original time. It returns
// ErrNotFound if there is no such key.
func (s *Store) RevokeKey(ctx context.Context, id int64) (time.Time, error) {
	revokedAt, err := s.q.RevokeAPIKey(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("revoke key %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("revoke key %d: %w", id, err)
	}
	return *revokedAt, nil
}
