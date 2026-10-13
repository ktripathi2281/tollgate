# Progress

**Current milestone:** M6, logs and observability. Done; waiting for the go-ahead for M7.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0. Postgres 18 in Docker (Docker Desktop with WSL integration).
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24), and a seventh during M3.
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes; OpenAI request validation, response types and error envelope; the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases; `POST /v1/chat/completions` and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap. CI green.
- M2: streaming end to end: the commit point, first-token, idle and total timeouts, SSE with error events after the commit point, usage estimates, a write deadline per event, and shutdown that drains streams then cancels stragglers. CI green.
- M3: the OpenAI-compatible and Anthropic adapters on raw net/http, with fixture tests; an SSE reader; error classification; per-target `sampling: false`; retries with backoff and full jitter, fallback, and a circuit breaker per target; 502/503/504 by outcome; the per-attempt story in the logs; providers without a key disabled; `make smoke`; the `fast` alias on Claude Haiku 5.5, and long-context price tiers. The manual smoke run waits for `ANTHROPIC_API_KEY` in `.env`.
- M4: Postgres through pgx, sqlc and embedded goose migrations; `tollgate migrate`; virtual keys with `tollgate keys create|list|revoke`; bearer auth with a 30s lookup cache; allowed aliases; per-key token buckets for requests and tokens per minute; Postgres-backed tests in per-test schemas, in CI against a service container. CI green.
- M5: cost in integer micro-USD, rounded up; per-key daily or monthly budgets (`keys create -budget -period`) with hold, settle and release in single transactions, and a sweeper for expired holds; `Idempotency-Key` with replay (streams included), 409, 422, takeover and retry after failure; the outcome saved before a non-streaming response is written, and before a stream's `[DONE]`; every exit path unwound by `defer` in reverse order; `tollgate reconcile`; startup checks on `hold_ttl` and the idempotency lock. CI green.
- M6: a request-log row per chat request, queued without blocking and written in batches with COPY, drained on shutdown; Prometheus metrics with bounded labels (requests by route and status, gateway overhead, time to first token, upstream outcomes, retries, fallbacks, breaker state, rejections, tokens and cost, replays, in-flight, dropped logs, settle failures); an admin listener on localhost for `/metrics` and pprof; `/readyz`; a Dockerfile, and compose with Postgres, migrations, the gateway and Prometheus.

## M6 acceptance

| Check | Status |
|---|---|
| The worker flushes by size and by time, drains on shutdown, and counts drops when the channel is full | pass: `TestBatchFlushesBySize`, `TestBatchFlushesByTime`, `TestBatchDrainsOnShutdown`, `TestRecordDropsWhenTheQueueIsFull` (internal/usage), `TestServeObservability` (cmd/tollgate, end to end) |
| Handlers never block on logging (test with a stalled database) | pass: `TestHandlersDoNotWaitForAStalledRequestLog` (internal/server), `TestRecordDoesNotWaitForAStalledDatabase` (internal/usage) |
| Metrics are visible in Prometheus through compose (manual) | pass, 2026-10-13: target `gateway:6060` up; request, upstream, token, breaker and in-flight series queried from Prometheus |

## Next

- Maintainer: review M6.
- Then: M7, benchmarks, docs and deployment.

## Open questions

- `fast` has one target, Claude Haiku 5.5, so it has nowhere to fall back to. An OpenAI target can be added once a model and its prices are chosen.

## Pending exercises

None. The M6 exercise, `batch`, was written on request.
