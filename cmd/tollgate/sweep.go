package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ktripathi2281/tollgate/internal/budget"
	"github.com/ktripathi2281/tollgate/internal/idempotency"
)

// sweepInterval is how often the sweeper runs. A hold is released at most
// this long after it expires.
const sweepInterval = time.Minute

// sweepEvery calls sweep at once, then every interval, until ctx is done.
func sweepEvery(ctx context.Context, interval time.Duration, sweep func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepOnce releases expired holds and deletes expired idempotency records,
// logging what it did.
func sweepOnce(ctx context.Context, b *budget.Budgets, k *idempotency.Keys, log *slog.Logger) {
	released, err := b.Sweep(ctx)
	if err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "sweeping expired holds failed", "error", err)
	}
	deleted, err := k.Sweep(ctx)
	if err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "sweeping expired idempotency records failed", "error", err)
	}
	if released > 0 || deleted > 0 {
		log.InfoContext(ctx, "swept", "holds_released", released, "idempotency_records_deleted", deleted)
	}
}
