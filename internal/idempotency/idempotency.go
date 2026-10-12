// Package idempotency implements the Idempotency-Key header (IETF draft
// draft-ietf-httpapi-idempotency-key-header): a client sends a key with a
// request, and a retry with the same key gets the first request's response
// instead of running it again. The rules are in brief section 9.2.
package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ktripathi2281/tollgate/internal/store"
)

// Header is the request header that carries the key.
const Header = "Idempotency-Key"

// maxKeyLen is the longest key accepted.
const maxKeyLen = 255

// ParseKey reads an Idempotency-Key header value. The draft makes it a
// Structured Field string, in double quotes; a bare value is accepted too,
// since many clients send one. Either way the key must be 1 to 255
// printable ASCII characters.
func ParseKey(v string) (string, error) {
	key := v
	if strings.HasPrefix(v, `"`) {
		var ok bool
		if key, ok = unquote(v); !ok {
			return "", errors.New("the Idempotency-Key header is not a valid quoted string")
		}
	}
	if key == "" || len(key) > maxKeyLen {
		return "", fmt.Errorf("an Idempotency-Key must be 1 to %d characters long", maxKeyLen)
	}
	for i := range len(key) {
		if key[i] < 0x20 || key[i] > 0x7e {
			return "", errors.New("an Idempotency-Key must be printable ASCII")
		}
	}
	return key, nil
}

// unquote decodes a Structured Field string (RFC 9651): characters between
// double quotes, where only \" and \\ are escapes.
func unquote(v string) (string, bool) {
	if len(v) < 2 || !strings.HasSuffix(v, `"`) {
		return "", false
	}
	var b strings.Builder
	in := v[1 : len(v)-1]
	for i := 0; i < len(in); i++ {
		switch c := in[i]; c {
		case '\\':
			i++
			if i == len(in) || (in[i] != '"' && in[i] != '\\') {
				return "", false
			}
			b.WriteByte(in[i])
		case '"':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// Store is what idempotency needs from the database. store.Store
// implements it.
type Store interface {
	InsertIdempotency(ctx context.Context, c store.IdemClaim) (bool, error)
	TakeOverIdempotency(ctx context.Context, c store.IdemClaim) (bool, error)
	GetIdempotency(ctx context.Context, keyID int64, idemKey string) (store.IdemRecord, error)
	CompleteIdempotency(ctx context.Context, keyID int64, idemKey, requestID string, status int, body []byte) (bool, error)
	DeleteIdempotency(ctx context.Context, keyID int64, idemKey, requestID string) (bool, error)
	DeleteExpiredIdempotency(ctx context.Context, now time.Time) (int64, error)
}

// Outcome is what Begin decided.
type Outcome int

const (
	// Proceed: the request owns the key. Run it, then Complete or Abandon.
	Proceed Outcome = iota
	// Replay: a request with this key and body completed. Send its response.
	Replay
	// InProgress: a request with this key is still running. 409.
	InProgress
	// Mismatch: the key was used with a different request body. 422.
	Mismatch
	// NotStored: a request with this key completed, but its response was
	// too big to store, so it can't be replayed. 422.
	NotStored
)

// Result is what Begin returns.
type Result struct {
	Outcome Outcome
	// Lock is the claim on the key, for Proceed.
	Lock *Lock
	// Status and Body are the stored response, for Replay.
	Status int
	Body   []byte
	// RetryAfter is when to try again, for InProgress.
	RetryAfter time.Duration
}

// Lock is a request's claim on an idempotency key.
type Lock struct {
	keyID     int64
	idemKey   string
	requestID string
}

// inProgressRetry is the Retry-After sent while a key's request is running.
const inProgressRetry = time.Second

// Keys handles idempotency keys. It is safe for concurrent use.
type Keys struct {
	store       Store
	lockFor     time.Duration
	retention   time.Duration
	maxResponse int
	now         func() time.Time
	log         *slog.Logger
}

// New returns a Keys. A claim on a key lasts lockFor before a retry may
// take it over; a completed response is kept for retention; responses
// over maxResponse bytes aren't stored. now is the clock; tests pass a
// fake one.
func New(s Store, lockFor, retention time.Duration, maxResponse int, now func() time.Time, log *slog.Logger) *Keys {
	return &Keys{store: s, lockFor: lockFor, retention: retention, maxResponse: maxResponse, now: now, log: log}
}

// Begin handles a request that sent an idempotency key, for the API key
// keyID. The request's identity is the SHA-256 hash of its raw body, so
// the same JSON serialised differently counts as a different request.
func (k *Keys) Begin(ctx context.Context, keyID int64, idemKey string, body []byte, requestID string) (Result, error) {
	hash := sha256.Sum256(body)
	// A record can change state between the claim and the read: it can
	// be deleted, completed or taken over. Each pass sees one state; three
	// passes are more than any real sequence of changes needs.
	for range 3 {
		now := k.now()
		claim := store.IdemClaim{
			KeyID: keyID, IdemKey: idemKey, RequestHash: hash, RequestID: requestID,
			Now: now, LockedUntil: now.Add(k.lockFor), ExpiresAt: now.Add(k.retention),
		}
		inserted, err := k.store.InsertIdempotency(ctx, claim)
		if err != nil {
			return Result{}, err
		}
		if inserted {
			return k.proceed(claim), nil
		}

		rec, err := k.store.GetIdempotency(ctx, keyID, idemKey)
		if errors.Is(err, store.ErrNotFound) {
			continue // removed since the claim failed; claim it again
		}
		if err != nil {
			return Result{}, err
		}
		expired := !now.Before(rec.ExpiresAt)
		abandoned := !rec.Completed && !now.Before(rec.LockedUntil) && rec.RequestHash == hash
		switch {
		case expired || abandoned:
			took, err := k.store.TakeOverIdempotency(ctx, claim)
			if err != nil {
				return Result{}, err
			}
			if took {
				if abandoned {
					k.log.WarnContext(ctx, "took over an abandoned idempotency key",
						"request_id", requestID, "abandoned_by", rec.RequestID)
				}
				return k.proceed(claim), nil
			}
			continue // another request took it first; look again
		case rec.RequestHash != hash:
			return Result{Outcome: Mismatch}, nil
		case !rec.Completed:
			return Result{Outcome: InProgress, RetryAfter: inProgressRetry}, nil
		case rec.ResponseBody == nil:
			return Result{Outcome: NotStored}, nil
		default:
			return Result{Outcome: Replay, Status: rec.ResponseStatus, Body: rec.ResponseBody}, nil
		}
	}
	return Result{}, errors.New("idempotency key changed state too often to claim")
}

func (k *Keys) proceed(c store.IdemClaim) Result {
	return Result{Outcome: Proceed, Lock: &Lock{keyID: c.KeyID, idemKey: c.IdemKey, requestID: c.RequestID}}
}

// Complete stores a successful response under the key, for replay. A
// response over the size cap completes the key without being stored. If
// the lock was lost, it was taken over after expiring, and the newer
// request's record is left alone.
func (k *Keys) Complete(ctx context.Context, l *Lock, status int, body []byte) error {
	stored := body
	if len(body) > k.maxResponse {
		stored = nil
	}
	ok, err := k.store.CompleteIdempotency(ctx, l.keyID, l.idemKey, l.requestID, status, stored)
	if err != nil {
		return err
	}
	if !ok {
		k.log.WarnContext(ctx, "the idempotency key was taken over before the request completed",
			"request_id", l.requestID)
	}
	return nil
}

// Abandon removes the key's record after a request failed, so the client
// can retry with the same key. Only successful responses are stored.
func (k *Keys) Abandon(ctx context.Context, l *Lock) error {
	_, err := k.store.DeleteIdempotency(ctx, l.keyID, l.idemKey, l.requestID)
	return err
}

// Sweep deletes the records past their retention and returns how many.
func (k *Keys) Sweep(ctx context.Context) (int64, error) {
	return k.store.DeleteExpiredIdempotency(ctx, k.now())
}
