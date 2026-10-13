-- InsertRequestLogs writes a batch of rows with Postgres's COPY protocol,
-- which is much faster than one INSERT per row.
-- name: InsertRequestLogs :copyfrom
INSERT INTO request_logs (
    created_at, request_id, key_id, alias, provider, model, stream, status, error_code, attempts,
    prompt_tokens, completion_tokens, usage_estimated, cost_micros, latency_us, ttft_us, replayed
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
);
