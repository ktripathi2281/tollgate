-- +goose Up

-- A key's optional spend budget: an amount per UTC day or month. Both are
-- set, or neither.
ALTER TABLE api_keys
    ADD COLUMN budget_micros bigint CHECK (budget_micros > 0),
    ADD COLUMN budget_period text   CHECK (budget_period IN ('daily', 'monthly')),
    ADD CONSTRAINT api_keys_budget_complete CHECK ((budget_micros IS NULL) = (budget_period IS NULL));

-- One row per key and budget period: what has been spent, and what is held
-- for requests still running. Holds and settles change both, in the same
-- transaction as the hold row they belong to.
CREATE TABLE budget_periods (
    key_id       bigint      NOT NULL REFERENCES api_keys (id),
    period_start timestamptz NOT NULL,
    spent_micros bigint      NOT NULL DEFAULT 0 CHECK (spent_micros >= 0),
    held_micros  bigint      NOT NULL DEFAULT 0 CHECK (held_micros >= 0),
    PRIMARY KEY (key_id, period_start)
);

-- Each request's hold, like a card authorisation. It starts held and
-- reaches exactly one of settled (with the actual cost) or released.
CREATE TABLE budget_holds (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key_id         bigint      NOT NULL,
    period_start   timestamptz NOT NULL,
    request_id     text        NOT NULL,
    amount_micros  bigint      NOT NULL CHECK (amount_micros > 0),
    settled_micros bigint      CHECK (settled_micros >= 0),
    status         text        NOT NULL CHECK (status IN ('held', 'settled', 'released')),
    created_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    resolved_at    timestamptz,
    FOREIGN KEY (key_id, period_start) REFERENCES budget_periods (key_id, period_start),
    -- The columns agree with the status: only a settled hold has a cost,
    -- and only a held one has no resolution time.
    CHECK ((status = 'settled') = (settled_micros IS NOT NULL)),
    CHECK ((status = 'held') = (resolved_at IS NULL))
);

-- The sweeper looks for expired holds that are still held.
CREATE INDEX budget_holds_expiring ON budget_holds (expires_at) WHERE status = 'held';

-- Idempotency-Key records, scoped to the API key. request_id is the request
-- that owns the record: only it may complete or remove it, so a request
-- whose lock was taken over can't overwrite its successor's work.
CREATE TABLE idempotency_records (
    key_id          bigint      NOT NULL REFERENCES api_keys (id),
    idem_key        text        NOT NULL CHECK (length(idem_key) BETWEEN 1 AND 255),
    request_hash    bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
    request_id      text        NOT NULL,
    status          text        NOT NULL CHECK (status IN ('in_progress', 'completed')),
    -- A completed record has its response, unless the response was too big
    -- to store.
    response_status integer,
    response_body   bytea,
    locked_until    timestamptz NOT NULL,
    created_at      timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    PRIMARY KEY (key_id, idem_key),
    CHECK ((status = 'completed') = (response_status IS NOT NULL))
);

-- The sweeper deletes records past their retention.
CREATE INDEX idempotency_records_expiring ON idempotency_records (expires_at);

-- +goose Down
DROP TABLE idempotency_records;
DROP TABLE budget_holds;
DROP TABLE budget_periods;
ALTER TABLE api_keys
    DROP CONSTRAINT api_keys_budget_complete,
    DROP COLUMN budget_period,
    DROP COLUMN budget_micros;
