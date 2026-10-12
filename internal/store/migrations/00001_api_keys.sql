-- +goose Up

-- Virtual API keys. Only a SHA-256 hash of each key is stored; the key
-- itself is shown once, when it is created. key_prefix is the start of the
-- key, kept so people can tell keys apart in listings.
CREATE TABLE api_keys (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name           text        NOT NULL CHECK (name <> ''),
    key_hash       bytea       NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    key_prefix     text        NOT NULL,
    rpm            integer     NOT NULL CHECK (rpm > 0),
    tpm            integer     NOT NULL CHECK (tpm > 0),
    -- NULL means every alias. An empty array would allow none, so it is
    -- rejected to keep "no restriction" spelled one way.
    allowed_models text[]      CHECK (cardinality(allowed_models) > 0),
    created_at     timestamptz NOT NULL DEFAULT now(),
    revoked_at     timestamptz
);

-- +goose Down
DROP TABLE api_keys;
