-- InsertIdempotencyRecord claims a key for a request. The primary key
-- decides races: of two requests with the same key, exactly one inserts.
-- name: InsertIdempotencyRecord :execrows
INSERT INTO idempotency_records
    (key_id, idem_key, request_hash, request_id, status, locked_until, created_at, expires_at)
VALUES (@key_id, @idem_key, @request_hash, @request_id, 'in_progress', @locked_until, @now, @expires_at)
ON CONFLICT (key_id, idem_key) DO NOTHING;

-- name: GetIdempotencyRecord :one
SELECT request_hash, request_id, status, response_status, response_body, locked_until, expires_at
FROM idempotency_records
WHERE key_id = @key_id AND idem_key = @idem_key;

-- TakeOverIdempotencyRecord claims a record for a new request if it is past
-- its retention, or if it is in progress with the same request hash and
-- its lock has run out: its request was abandoned, say by a crash. The
-- conditions are in the WHERE clause, so of two requests trying at once,
-- exactly one succeeds.
-- name: TakeOverIdempotencyRecord :execrows
UPDATE idempotency_records
SET request_hash = @request_hash, request_id = @request_id, status = 'in_progress',
    response_status = NULL, response_body = NULL,
    locked_until = @locked_until, created_at = @now, expires_at = @expires_at
WHERE key_id = @key_id AND idem_key = @idem_key
  AND (expires_at <= @now
       OR (status = 'in_progress' AND locked_until <= @now AND request_hash = @request_hash));

-- name: CompleteIdempotencyRecord :execrows
UPDATE idempotency_records
SET status = 'completed', response_status = @response_status, response_body = @response_body
WHERE key_id = @key_id AND idem_key = @idem_key AND request_id = @request_id AND status = 'in_progress';

-- name: DeleteIdempotencyRecord :execrows
DELETE FROM idempotency_records
WHERE key_id = @key_id AND idem_key = @idem_key AND request_id = @request_id AND status = 'in_progress';

-- name: DeleteExpiredIdempotencyRecords :execrows
DELETE FROM idempotency_records WHERE expires_at <= @now;
