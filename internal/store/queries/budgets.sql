-- name: EnsureBudgetPeriod :exec
INSERT INTO budget_periods (key_id, period_start)
VALUES (@key_id, @period_start)
ON CONFLICT (key_id, period_start) DO NOTHING;

-- PlaceHold adds a hold to the period's held amount, only if
-- spent + held + amount stays within the budget, and inserts the hold row,
-- in one statement. It returns no row if the hold doesn't fit.
--
-- The UPDATE locks the period's row, so concurrent holds for one key take
-- turns, and each re-checks the condition against the last one's result.
-- Being one statement, it holds that lock only while Postgres runs it: no
-- network round trip happens in between, which is what lets one key take
-- thousands of holds a second (DECISIONS 59).
-- name: PlaceHold :one
WITH reserved AS (
    UPDATE budget_periods bp
    SET held_micros = bp.held_micros + @amount::bigint
    WHERE bp.key_id = @key_id::bigint
      AND bp.period_start = @period_start::timestamptz
      AND bp.spent_micros + bp.held_micros + @amount::bigint <= @budget::bigint
    RETURNING bp.key_id, bp.period_start
)
INSERT INTO budget_holds (key_id, period_start, request_id, amount_micros, status, created_at, expires_at)
SELECT r.key_id, r.period_start, @request_id::text, @amount::bigint, 'held', @now::timestamptz, @expires_at::timestamptz
FROM reserved r
RETURNING id;

-- SettleHold and ReleaseHold move a hold out of 'held' and update its
-- period, in one statement: held loses the hold's amount, and spent gains
-- the cost (nothing, for a release). The status guard makes them no-ops on
-- a hold already resolved, so each hold reaches exactly one terminal state,
-- once. They return no row then.
-- name: SettleHold :one
WITH settled AS (
    UPDATE budget_holds h
    SET status = 'settled', settled_micros = @cost::bigint, resolved_at = @now::timestamptz
    WHERE h.id = @id::bigint AND h.status = 'held'
    RETURNING h.key_id, h.period_start, h.amount_micros
)
UPDATE budget_periods p
SET held_micros = p.held_micros - s.amount_micros,
    spent_micros = p.spent_micros + @cost::bigint
FROM settled s
WHERE p.key_id = s.key_id AND p.period_start = s.period_start
RETURNING s.amount_micros;

-- name: ReleaseHold :one
WITH released AS (
    UPDATE budget_holds h
    SET status = 'released', resolved_at = @now::timestamptz
    WHERE h.id = @id::bigint AND h.status = 'held'
    RETURNING h.key_id, h.period_start, h.amount_micros
)
UPDATE budget_periods p
SET held_micros = p.held_micros - r.amount_micros
FROM released r
WHERE p.key_id = r.key_id AND p.period_start = r.period_start
RETURNING r.amount_micros;

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
