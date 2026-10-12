# Progress

**Current milestone:** M4, keys and limits. Done; waiting for the go-ahead for M5.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0. Postgres 18 in Docker (Docker Desktop with WSL integration).
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24), and a seventh during M3.
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes; OpenAI request validation, response types and error envelope; the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases; `POST /v1/chat/completions` and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap. CI green.
- M2: streaming end to end: the commit point, first-token, idle and total timeouts, SSE with error events after the commit point, usage estimates, a write deadline per event, and shutdown that drains streams then cancels stragglers. CI green.
- M3: the OpenAI-compatible and Anthropic adapters on raw net/http, with fixture tests; an SSE reader; error classification; per-target `sampling: false`; retries with backoff and full jitter, fallback, and a circuit breaker per target; 502/503/504 by outcome; the per-attempt story in the logs; providers without a key disabled; `make smoke`; the `fast` alias on Claude Haiku 5.5, and long-context price tiers. The manual smoke run waits for `ANTHROPIC_API_KEY` in `.env`.
- M4: Postgres through pgx, sqlc and embedded goose migrations; `tollgate migrate`, and `serve` refusing an out-of-date schema; virtual keys (`tg_` plus 256 random bits, stored as SHA-256) with `tollgate keys create|list|revoke`; bearer auth with a 30s lookup cache; allowed aliases; per-key token buckets for requests and tokens per minute, with reservations refunded on every exit path; `docker-compose.yml` with Postgres; Postgres-backed tests in per-test schemas, run in CI against a service container.

## M4 acceptance

| Check | Status |
|---|---|
| 401 without a valid key | pass: `TestUnauthenticatedRequestsGet401` (internal/server), `TestServeExitsCleanlyOnSIGTERM` (cmd/tollgate, end to end) |
| A revoked key stops working within the cache TTL | pass: `TestCacheRevocationTakesEffectWithinTTL` (internal/auth) |
| N goroutines hammering one key under a frozen clock: exactly what the bucket allows is admitted | pass: `TestRegistryAdmitsExactlyTheLimitUnderContention` (internal/limiter) |
| Token reservations refunded on success, failure and cancellation | pass: `TestTokenReservationsAreRefunded` (internal/server) |

## Next

- Maintainer: review M4.
- Then: M5, the money path.

## Open questions

- `fast` has one target, Claude Haiku 5.5, so it has nowhere to fall back to. An OpenAI target can be added once a model and its prices are chosen.

## Pending exercises

None. The M4 exercise, `bucket.take`, was written on request.
