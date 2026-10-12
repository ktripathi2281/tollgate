# Progress

**Current milestone:** M5, the money path. Done; waiting for the go-ahead for M6.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0. Postgres 18 in Docker (Docker Desktop with WSL integration).
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24), and a seventh during M3.
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes; OpenAI request validation, response types and error envelope; the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases; `POST /v1/chat/completions` and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap. CI green.
- M2: streaming end to end: the commit point, first-token, idle and total timeouts, SSE with error events after the commit point, usage estimates, a write deadline per event, and shutdown that drains streams then cancels stragglers. CI green.
- M3: the OpenAI-compatible and Anthropic adapters on raw net/http, with fixture tests; an SSE reader; error classification; per-target `sampling: false`; retries with backoff and full jitter, fallback, and a circuit breaker per target; 502/503/504 by outcome; the per-attempt story in the logs; providers without a key disabled; `make smoke`; the `fast` alias on Claude Haiku 5.5, and long-context price tiers. The manual smoke run waits for `ANTHROPIC_API_KEY` in `.env`.
- M4: Postgres through pgx, sqlc and embedded goose migrations; `tollgate migrate`; virtual keys with `tollgate keys create|list|revoke`; bearer auth with a 30s lookup cache; allowed aliases; per-key token buckets for requests and tokens per minute; Postgres-backed tests in per-test schemas, in CI against a service container. CI green.
- M5: cost in integer micro-USD, rounded up; per-key daily or monthly budgets (`keys create -budget -period`) with hold, settle and release in single transactions, and a sweeper for expired holds; `Idempotency-Key` with replay (streams included), 409, 422, takeover and retry after failure; the outcome saved before a non-streaming response is written, and before a stream's `[DONE]`; every exit path unwound by `defer` in reverse order; `tollgate reconcile`; startup checks on `hold_ttl` and the idempotency lock.

## M5 acceptance

| Check | Status |
|---|---|
| 200 concurrent requests, budget fits exactly K holds: K succeed, the rest 402, invariants hold | pass: `TestBudgetAdmitsExactlyKOf200` (internal/server) |
| Every exit path (success, upstream failure, failure after the hold, client disconnect, panic) leaves no hold held | pass: `TestEveryExitPathResolvesTheHold` (internal/server) |
| A hold orphaned by a simulated crash is released by the sweeper | pass: `TestSweeperAndLateSettle` (internal/budget) |
| Double settle and settle-after-release are no-ops; a late settle is logged and counted | pass: `TestResolvingTwiceChangesNothing`, `TestSweeperAndLateSettle` (internal/budget) |
| Startup rejects a `hold_ttl` or idempotency lock too short for `timeouts.total` | pass: `TestParseRejectsTimingsTooShortForARequest` (internal/config) |
| A hold placed before a period boundary settles into its own period; the new period starts at zero | pass: `TestPeriodBoundary` (internal/budget) |
| Idempotency: replay, 422, 409, retry after failure, takeover after `locked_until`, N duplicates make one upstream call | pass: `TestIdempotentStreamReplay`, `TestIdempotencyMismatchAndBadKey`, `TestConcurrentDuplicatesCallTheProviderOnce`, `TestRetryAfterAFailure`, `TestTakeoverOfAnAbandonedKey`, `TestIdempotentReplay` (internal/server), and `internal/idempotency` |
| `tollgate reconcile` passes after all of the above and fails when a row is tampered with | pass: `TestReconcile` (cmd/tollgate); every money test ends with the same check (`storetest.CheckBudgets`); `TestServeSpendsABudget` (end to end) |

## Next

- Maintainer: review M5.
- Then: M6, logs and observability.

## Open questions

- `fast` has one target, Claude Haiku 5.5, so it has nowhere to fall back to. An OpenAI target can be added once a model and its prices are chosen.

## Pending exercises

None. The M5 exercise, `usage.Cost`, was written on request.
