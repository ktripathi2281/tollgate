// Package budget enforces per-key spend budgets the way card payments are
// authorised and captured: before a request runs, a hold reserves its
// worst-case cost; afterwards the hold is settled with the actual cost, or
// released if nothing was spent. A sweeper releases holds that were never
// resolved, like expired authorisations. The rules are in brief section 9.1.
package budget

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/store"
)

// Budget periods. A period starts at midnight UTC, on the first of the
// month for a monthly budget.
const (
	Daily   = "daily"
	Monthly = "monthly"
)

// PeriodStart returns the start of the period that contains t.
func PeriodStart(period string, t time.Time) time.Time {
	t = t.UTC()
	if period == Monthly {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// PeriodEnd returns the end of the period that starts at start, which is
// the start of the next one.
func PeriodEnd(period string, start time.Time) time.Time {
	if period == Monthly {
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// ErrExceeded is returned by Hold when the hold doesn't fit in what is left
// of the key's budget for the period.
var ErrExceeded = errors.New("budget: the key's budget can't cover this request")

// Store is what budgets need from the database. store.Store implements it.
type Store interface {
	PlaceHold(ctx context.Context, p store.HoldParams) (int64, error)
	SettleHold(ctx context.Context, id int64, cost money.Micros, now time.Time) (store.Resolution, error)
	ReleaseHold(ctx context.Context, id int64, now time.Time) (store.Resolution, error)
	ExpiredHolds(ctx context.Context, now time.Time, limit int32) ([]int64, error)
}

// Hold is a placed hold.
type Hold struct {
	ID          int64
	KeyID       int64
	PeriodStart time.Time
	Amount      money.Micros
	RequestID   string
}

// Stats counts the events that mean spend went unrecorded or a hold was
// left behind. Milestone 6 exports them as metrics.
type Stats struct {
	// SettleFailures counts settles that failed after every retry. The
	// hold stays held until the sweeper releases it, so its cost is never
	// recorded as spent.
	SettleFailures atomic.Int64
	// LateSettles counts settles that found their hold already released by
	// the sweeper. They change nothing, so their cost isn't recorded.
	LateSettles atomic.Int64
	// Swept counts expired holds the sweeper released.
	Swept atomic.Int64
}

// Settle and release attempts: the first try and two retries.
const (
	attempts   = 3
	retryAfter = 100 * time.Millisecond
)

// Budgets places and resolves holds. It is safe for concurrent use.
type Budgets struct {
	store   Store
	holdTTL time.Duration
	now     func() time.Time
	log     *slog.Logger
	// retryWait is the pause between settle or release attempts; tests
	// shorten it.
	retryWait time.Duration

	Stats Stats
}

// New returns a Budgets whose holds expire after holdTTL. now is the clock;
// tests pass a fake one.
func New(s Store, holdTTL time.Duration, now func() time.Time, log *slog.Logger) *Budgets {
	return &Budgets{store: s, holdTTL: holdTTL, now: now, log: log, retryWait: retryAfter}
}

// Hold reserves amount from the key's budget for the current period. It
// returns nil and no error if there is nothing to hold: the key has no
// budget, or the request can't cost anything. It returns ErrExceeded if
// the budget can't cover the hold.
func (b *Budgets) Hold(ctx context.Context, key auth.Key, amount money.Micros, requestID string) (*Hold, error) {
	if key.Budget <= 0 || amount <= 0 {
		return nil, nil
	}
	now := b.now()
	h := &Hold{KeyID: key.ID, PeriodStart: PeriodStart(key.BudgetPeriod, now), Amount: amount, RequestID: requestID}
	id, err := b.store.PlaceHold(ctx, store.HoldParams{
		KeyID: key.ID, PeriodStart: h.PeriodStart, RequestID: requestID,
		Amount: amount, Budget: key.Budget, Now: now, ExpiresAt: now.Add(b.holdTTL),
	})
	if errors.Is(err, store.ErrBudgetExceeded) {
		return nil, ErrExceeded
	}
	if err != nil {
		return nil, err
	}
	h.ID = id
	return h, nil
}

// Settle records what a held request actually cost, even if that is more
// than the hold or takes the key past its budget: the tokens were used,
// so they're owed. The cost goes to the hold's own period, even if a new
// period has started since.
//
// A failed settle is retried. If every attempt fails, Settle logs the
// amounts at error level and counts it: the hold stays held until the
// sweeper releases it, and the cost is never recorded. That is the known
// limitation of the settle window. A settle that finds the hold already
// released by the sweeper changes nothing; it is logged and counted too.
func (b *Budgets) Settle(ctx context.Context, h *Hold, cost money.Micros) {
	res, err := b.resolve(ctx, func(now time.Time) (store.Resolution, error) {
		return b.store.SettleHold(ctx, h.ID, cost, now)
	})
	switch {
	case err != nil:
		b.Stats.SettleFailures.Add(1)
		b.log.ErrorContext(ctx, "settle failed; the cost is not recorded",
			"request_id", h.RequestID, "hold_id", h.ID, "key_id", h.KeyID,
			"hold_micros", int64(h.Amount), "cost_micros", int64(cost), "error", err)
	case res == store.AlreadyReleased:
		b.Stats.LateSettles.Add(1)
		b.log.ErrorContext(ctx, "late settle: the sweeper had already released the hold; the cost is not recorded",
			"request_id", h.RequestID, "hold_id", h.ID, "key_id", h.KeyID,
			"hold_micros", int64(h.Amount), "cost_micros", int64(cost))
	}
}

// Release gives back a hold for a request that spent nothing. It retries
// like Settle; if every attempt fails, it logs at error level and leaves
// the hold for the sweeper, which releases it once it expires.
func (b *Budgets) Release(ctx context.Context, h *Hold) {
	_, err := b.resolve(ctx, func(now time.Time) (store.Resolution, error) {
		return b.store.ReleaseHold(ctx, h.ID, now)
	})
	if err != nil {
		b.log.ErrorContext(ctx, "release failed; the sweeper will release the hold when it expires",
			"request_id", h.RequestID, "hold_id", h.ID, "key_id", h.KeyID,
			"hold_micros", int64(h.Amount), "error", err)
	}
}

// resolve runs one settle or release, retrying on failure. Retrying is
// safe because both are no-ops on a hold that is no longer held.
func (b *Budgets) resolve(ctx context.Context, op func(now time.Time) (store.Resolution, error)) (store.Resolution, error) {
	var err error
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, errors.Join(err, ctx.Err())
			case <-time.After(b.retryWait):
			}
		}
		var res store.Resolution
		res, err = op(b.now())
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return res, err
		}
	}
	return 0, err
}

// sweepBatch is how many expired holds one sweep looks up at a time.
const sweepBatch = 100

// Sweep releases every hold that has passed its expiry while still held,
// and returns how many it released. This is the crash-recovery path: a
// hold is only left held this long if its request never finished.
func (b *Budgets) Sweep(ctx context.Context) (int, error) {
	released := 0
	for {
		now := b.now()
		ids, err := b.store.ExpiredHolds(ctx, now, sweepBatch)
		if err != nil {
			return released, err
		}
		for _, id := range ids {
			res, err := b.store.ReleaseHold(ctx, id, now)
			if err != nil {
				return released, err
			}
			if res == store.Resolved {
				released++
				b.Stats.Swept.Add(1)
				b.log.WarnContext(ctx, "released an expired hold", "hold_id", id)
			}
		}
		if len(ids) < sweepBatch {
			return released, nil
		}
	}
}
