package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ktripathi2281/tollgate/internal/store/db"
)

// IdemClaim is a request claiming an idempotency key.
type IdemClaim struct {
	KeyID       int64
	IdemKey     string
	RequestHash [sha256.Size]byte
	RequestID   string // the claiming request, which then owns the record
	Now         time.Time
	LockedUntil time.Time
	ExpiresAt   time.Time
}

// IdemRecord is a stored idempotency record.
type IdemRecord struct {
	RequestHash [sha256.Size]byte
	RequestID   string
	Completed   bool
	// ResponseStatus and ResponseBody are set once the record is completed.
	// ResponseBody is nil if the response was too big to store.
	ResponseStatus int
	ResponseBody   []byte
	LockedUntil    time.Time
	ExpiresAt      time.Time
}

// InsertIdempotency claims a key that has no record. It returns false if
// there is already one, whatever its state.
func (s *Store) InsertIdempotency(ctx context.Context, c IdemClaim) (bool, error) {
	n, err := s.q.InsertIdempotencyRecord(ctx, db.InsertIdempotencyRecordParams{
		KeyID: c.KeyID, IdemKey: c.IdemKey, RequestHash: c.RequestHash[:], RequestID: c.RequestID,
		LockedUntil: c.LockedUntil, Now: c.Now, ExpiresAt: c.ExpiresAt,
	})
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return n == 1, nil
}

// TakeOverIdempotency claims a key whose record is past its retention, or
// in progress with the same request hash and an expired lock. It returns
// false if the record is in neither state, or another request got there
// first.
func (s *Store) TakeOverIdempotency(ctx context.Context, c IdemClaim) (bool, error) {
	n, err := s.q.TakeOverIdempotencyRecord(ctx, db.TakeOverIdempotencyRecordParams{
		RequestHash: c.RequestHash[:], RequestID: c.RequestID,
		LockedUntil: c.LockedUntil, Now: c.Now, ExpiresAt: c.ExpiresAt,
		KeyID: c.KeyID, IdemKey: c.IdemKey,
	})
	if err != nil {
		return false, fmt.Errorf("take over idempotency key: %w", err)
	}
	return n == 1, nil
}

// GetIdempotency returns a key's record, or ErrNotFound.
func (s *Store) GetIdempotency(ctx context.Context, keyID int64, idemKey string) (IdemRecord, error) {
	row, err := s.q.GetIdempotencyRecord(ctx, db.GetIdempotencyRecordParams{KeyID: keyID, IdemKey: idemKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return IdemRecord{}, ErrNotFound
	}
	if err != nil {
		return IdemRecord{}, fmt.Errorf("read idempotency record: %w", err)
	}
	rec := IdemRecord{
		RequestID:    row.RequestID,
		Completed:    row.Status == "completed",
		ResponseBody: row.ResponseBody,
		LockedUntil:  row.LockedUntil,
		ExpiresAt:    row.ExpiresAt,
	}
	copy(rec.RequestHash[:], row.RequestHash)
	if row.ResponseStatus != nil {
		rec.ResponseStatus = int(*row.ResponseStatus)
	}
	return rec, nil
}

// CompleteIdempotency stores the response of the request that owns the
// record. A nil body records that the response was too big to store. It
// returns false if the record isn't in progress under requestID: it was
// taken over, or never claimed.
func (s *Store) CompleteIdempotency(ctx context.Context, keyID int64, idemKey, requestID string, status int, body []byte) (bool, error) {
	if status < 100 || status > math.MaxInt16 {
		return false, fmt.Errorf("complete idempotency key: status %d is not an HTTP status", status)
	}
	n, err := s.q.CompleteIdempotencyRecord(ctx, db.CompleteIdempotencyRecordParams{
		ResponseStatus: new(int32(status)), ResponseBody: body,
		KeyID: keyID, IdemKey: idemKey, RequestID: requestID,
	})
	if err != nil {
		return false, fmt.Errorf("complete idempotency key: %w", err)
	}
	return n == 1, nil
}

// DeleteIdempotency removes the in-progress record that requestID owns, so
// the key can be used again. It returns false if there was none.
func (s *Store) DeleteIdempotency(ctx context.Context, keyID int64, idemKey, requestID string) (bool, error) {
	n, err := s.q.DeleteIdempotencyRecord(ctx, db.DeleteIdempotencyRecordParams{
		KeyID: keyID, IdemKey: idemKey, RequestID: requestID,
	})
	if err != nil {
		return false, fmt.Errorf("remove idempotency key: %w", err)
	}
	return n == 1, nil
}

// DeleteExpiredIdempotency removes every record past its retention at now,
// and returns how many there were.
func (s *Store) DeleteExpiredIdempotency(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.DeleteExpiredIdempotencyRecords(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired idempotency records: %w", err)
	}
	return n, nil
}
