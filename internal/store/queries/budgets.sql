-- name: EnsureBudgetPeriod :exec
INSERT INTO budget_periods (key_id, period_start)
VALUES (@key_id, @period_start)
ON CONFLICT (key_id, period_start) DO NOTHING;

-- ReserveBudget adds a hold to the period's held amount, only if
-- spent + held + amount stays within the budget. The UPDATE locks the row,
-- so concurrent holds for one key take turns, and each sees the last one's
-- amount: the condition can't be passed twice on stale numbers.
-- name: ReserveBudget :execrows
UPDATE budget_periods
SET held_micros = held_micros + @amount::bigint
WHERE key_id = @key_id
  AND period_start = @period_start
  AND spent_micros + held_micros + @amount::bigint <= @budget::bigint;

-- name: InsertHold :one
INSERT INTO budget_holds (key_id, period_start, request_id, amount_micros, status, created_at, expires_at)
VALUES (@key_id, @period_start, @request_id, @amount, 'held', @now, @expires_at)
RETURNING id;

-- SettleHold and ReleaseHold move a hold out of 'held'. The status guard
-- makes them no-ops on a hold that has already been resolved, so each hold
-- reaches exactly one terminal state, once.
-- name: SettleHold :one
UPDATE budget_holds
SET status = 'settled', settled_micros = @cost::bigint, resolved_at = @now::timestamptz
WHERE id = @id AND status = 'held'
RETURNING key_id, period_start, amount_micros;

-- name: ReleaseHold :one
UPDATE budget_holds
SET status = 'released', resolved_at = @now::timestamptz
WHERE id = @id AND status = 'held'
RETURNING key_id, period_start, amount_micros;

-- ResolveHoldInPeriod takes a resolved hold's amount out of held, and adds
-- its cost (0 for a release) to spent.
-- name: ResolveHoldInPeriod :exec
UPDATE budget_periods
SET held_micros = held_micros - @amount::bigint,
    spent_micros = spent_micros + @cost::bigint
WHERE key_id = @key_id AND period_start = @period_start;

-- name: GetHoldStatus :one
SELECT status FROM budget_holds WHERE id = @id;

-- name: ExpiredHolds :many
SELECT id FROM budget_holds
WHERE status = 'held' AND expires_at <= @now
ORDER BY expires_at
LIMIT @max_rows;

-- name: GetBudgetPeriod :one
SELECT spent_micros, held_micros FROM budget_periods
WHERE key_id = @key_id AND period_start = @period_start;

-- ReconcileBudgets returns every period's totals next to the sums of its
-- holds, for `tollgate reconcile`. One statement sees one snapshot, so
-- requests running meanwhile can't make the two disagree.
-- name: ReconcileBudgets :many
SELECT p.key_id, p.period_start, p.held_micros, p.spent_micros,
       COALESCE(SUM(h.amount_micros) FILTER (WHERE h.status = 'held'), 0)::bigint AS holds_held,
       COALESCE(SUM(h.settled_micros) FILTER (WHERE h.status = 'settled'), 0)::bigint AS holds_settled
FROM budget_periods p
LEFT JOIN budget_holds h ON h.key_id = p.key_id AND h.period_start = p.period_start
GROUP BY p.key_id, p.period_start
ORDER BY p.key_id, p.period_start;
