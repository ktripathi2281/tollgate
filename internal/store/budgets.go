package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/store/db"
)

// ErrBudgetExceeded is returned by PlaceHold when the hold doesn't fit in
// what is left of the budget.
var ErrBudgetExceeded = errors.New("store: budget exceeded")

// HoldParams describes a hold to place.
type HoldParams struct {
	KeyID       int64
	PeriodStart time.Time
	RequestID   string
	Amount      money.Micros // must be positive
	Budget      money.Micros // the key's budget per period
	Now         time.Time
	ExpiresAt   time.Time
}

// PlaceHold reserves p.Amount of the key's budget for the period and
// returns the hold's ID. It makes sure the period's row exists, then, in
// one statement, adds the amount to held only if spent + held + amount
// stays within the budget, and inserts the hold. It returns
// ErrBudgetExceeded, and changes nothing, if the hold doesn't fit.
func (s *Store) PlaceHold(ctx context.Context, p HoldParams) (int64, error) {
	err := s.q.EnsureBudgetPeriod(ctx, db.EnsureBudgetPeriodParams{KeyID: p.KeyID, PeriodStart: p.PeriodStart})
	if err != nil {
		return 0, fmt.Errorf("place hold: %w", err)
	}
	id, err := s.q.PlaceHold(ctx, db.PlaceHoldParams{
		RequestID: p.RequestID, Amount: int64(p.Amount), Now: p.Now, ExpiresAt: p.ExpiresAt,
		KeyID: p.KeyID, PeriodStart: p.PeriodStart, Budget: int64(p.Budget),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("place hold: %w", ErrBudgetExceeded)
	}
	if err != nil {
		return 0, fmt.Errorf("place hold: %w", err)
	}
	return id, nil
}

// Resolution is what a call to SettleHold or ReleaseHold found.
type Resolution int

const (
	// Resolved: the hold was held, and this call settled or released it.
	Resolved Resolution = iota
	// AlreadySettled and AlreadyReleased: an earlier call resolved the
	// hold, and this one changed nothing.
	AlreadySettled
	AlreadyReleased
)

// SettleHold marks a hold settled with its actual cost and, in the same
// statement, takes the hold's amount out of the period's held and adds the
// cost to its spent. The cost may be more than the hold. It returns
// ErrNotFound if there is no such hold.
func (s *Store) SettleHold(ctx context.Context, id int64, cost money.Micros, now time.Time) (Resolution, error) {
	_, err := s.q.SettleHold(ctx, db.SettleHoldParams{Cost: int64(cost), Now: now, ID: id})
	return s.resolution(ctx, "settle", id, err)
}

// ReleaseHold marks a hold released and, in the same statement, takes its
// amount out of the period's held. It returns ErrNotFound if there is no
// such hold.
func (s *Store) ReleaseHold(ctx context.Context, id int64, now time.Time) (Resolution, error) {
	_, err := s.q.ReleaseHold(ctx, db.ReleaseHoldParams{Now: now, ID: id})
	return s.resolution(ctx, "release", id, err)
}

// resolution interprets the result of a settle or release. No row means
// the status guard found the hold already resolved, or missing; a terminal
// status never changes, so reading it afterwards is safe.
func (s *Store) resolution(ctx context.Context, verb string, id int64, err error) (Resolution, error) {
	if err == nil {
		return Resolved, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%s hold %d: %w", verb, id, err)
	}
	status, err := s.q.GetHoldStatus(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("%s hold %d: %w", verb, id, ErrNotFound)
	case err != nil:
		return 0, fmt.Errorf("%s hold %d: %w", verb, id, err)
	case status == "settled":
		return AlreadySettled, nil
	default:
		return AlreadyReleased, nil
	}
}

// ExpiredHolds returns up to limit holds still held at now whose expiry
// has passed, oldest first.
func (s *Store) ExpiredHolds(ctx context.Context, now time.Time, limit int32) ([]int64, error) {
	ids, err := s.q.ExpiredHolds(ctx, db.ExpiredHoldsParams{Now: now, MaxRows: limit})
	if err != nil {
		return nil, fmt.Errorf("find expired holds: %w", err)
	}
	return ids, nil
}

// BudgetPeriod returns a key's spent and held amounts for the period that
// starts at start: zero if the period has no row yet.
func (s *Store) BudgetPeriod(ctx context.Context, keyID int64, start time.Time) (spent, held money.Micros, err error) {
	row, err := s.q.GetBudgetPeriod(ctx, db.GetBudgetPeriodParams{KeyID: keyID, PeriodStart: start})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read budget period: %w", err)
	}
	return money.Micros(row.SpentMicros), money.Micros(row.HeldMicros), nil
}

// PeriodCheck is one budget period's totals next to the sums of its holds.
type PeriodCheck struct {
	KeyID        int64
	PeriodStart  time.Time
	Held         money.Micros // the period's held_micros
	Spent        money.Micros // the period's spent_micros
	HoldsHeld    money.Micros // the sum of its holds still held
	HoldsSettled money.Micros // the sum of the costs of its settled holds
}

// OK reports whether the period agrees with its holds: invariants 1 and 2
// in brief section 9.1.
func (c PeriodCheck) OK() bool {
	return c.Held == c.HoldsHeld && c.Spent == c.HoldsSettled
}

// ReconcileBudgets returns every budget period with the sums of its holds,
// read in one snapshot.
func (s *Store) ReconcileBudgets(ctx context.Context) ([]PeriodCheck, error) {
	rows, err := s.q.ReconcileBudgets(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile budgets: %w", err)
	}
	checks := make([]PeriodCheck, 0, len(rows))
	for _, r := range rows {
		checks = append(checks, PeriodCheck{
			KeyID: r.KeyID, PeriodStart: r.PeriodStart,
			Held: money.Micros(r.HeldMicros), Spent: money.Micros(r.SpentMicros),
			HoldsHeld: money.Micros(r.HoldsHeld), HoldsSettled: money.Micros(r.HoldsSettled),
		})
	}
	return checks, nil
}
