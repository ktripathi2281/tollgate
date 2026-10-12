-- name: CreateAPIKey :one
INSERT INTO api_keys (name, key_hash, key_prefix, rpm, tpm, allowed_models, budget_micros, budget_period)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at;

-- name: GetAPIKeyByHash :one
SELECT id, name, key_prefix, rpm, tpm, allowed_models, budget_micros, budget_period, revoked_at
FROM api_keys
WHERE key_hash = $1;

-- name: ListAPIKeys :many
SELECT id, name, key_prefix, rpm, tpm, allowed_models, budget_micros, budget_period, created_at, revoked_at
FROM api_keys
ORDER BY id;

-- RevokeAPIKey is idempotent: revoking a revoked key keeps its first
-- revocation time. It returns no row if the key doesn't exist.
-- name: RevokeAPIKey :one
UPDATE api_keys
SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1
RETURNING revoked_at;
