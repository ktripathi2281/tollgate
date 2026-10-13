-- +goose Up

-- One row per chat request, written in batches by the request-log worker.
-- Rows can be dropped under overload, so nothing about money depends on
-- this table: budgets have their own. There is no foreign key to api_keys,
-- which keeps the bulk insert cheap; key_id is NULL for requests without a
-- valid key, and the columns about the upstream are NULL when no provider
-- was called.
CREATE TABLE request_logs (
    id                bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at        timestamptz NOT NULL,
    request_id        text        NOT NULL,
    key_id            bigint,
    alias             text,
    provider          text,
    model             text,
    stream            boolean     NOT NULL,
    status            integer     NOT NULL,
    -- The error code sent to the client, if the request failed.
    error_code        text,
    attempts          integer     NOT NULL,
    prompt_tokens     integer     NOT NULL,
    completion_tokens integer     NOT NULL,
    usage_estimated   boolean     NOT NULL,
    cost_micros       bigint      NOT NULL,
    latency_us        bigint      NOT NULL,
    -- Time to the first content, for streams that sent any.
    ttft_us           bigint,
    replayed          boolean     NOT NULL
);

CREATE INDEX request_logs_created_at ON request_logs (created_at);
CREATE INDEX request_logs_key ON request_logs (key_id, created_at);

-- +goose Down
DROP TABLE request_logs;
