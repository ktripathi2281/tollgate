# Progress

**Current milestone:** M3, real providers and routing. Built; waiting for the maintainer's model IDs and prices, the smoke run, and the M3 exercise.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0.
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24).
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes; OpenAI request validation, response types and error envelope; the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases; `POST /v1/chat/completions` and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap. CI green.
- M2: streaming end to end: the commit point, first-token, idle and total timeouts, SSE with error events after the commit point, usage estimates, a write deadline per event, and shutdown that drains streams then cancels stragglers. CI green.
- M3: the OpenAI-compatible and Anthropic adapters on raw net/http, with fixture tests; an SSE reader; error classification (quota 429s and upstream 404/402 are config errors); provider-specific request rules and per-target `sampling: false`; retries with backoff and full jitter, short `Retry-After` honoured, fallback, and a circuit breaker per target; 502/503/504 by outcome; the per-attempt story in the logs; providers without a key disabled with a warning; `make smoke` and `.env.example`.

## M3 acceptance

| Check | Status |
|---|---|
| Fixture tests for both adapters, streaming and not: usage, stop reasons, pings, unknown events, an error mid-stream | pass: `internal/provider/openai`, `internal/provider/anthropic` |
| Router matrix: retry then succeed, fall back, bad request not retried, short and long `Retry-After`, cancelled, everything failed | pass: `TestRouterMatrix`, `TestRouterCancelled`, `TestRouterExhausted` (internal/router) |
| Breaker opens, half-opens, closes, and stays correct under concurrent callers | pass: `TestBreaker*` (internal/router) |
| `make smoke` passes against the real APIs (manual) | not run yet: needs keys and model IDs in `.env` |

## Next

- Maintainer: send the model IDs and prices for the `fast` alias; put the keys and smoke models in `.env`; implement `backoff`; review M3.
- Then: add the `fast` alias to `config.yaml`, run `make smoke`, and stop for M4.

## Open questions

- Which OpenAI and Anthropic models back `fast`, at what prices, and do they accept sampling parameters? (Current Claude models don't.)

## Pending exercises

- **M3: `backoff`** in `internal/router/backoff.go`. Tests: `internal/router/backoff_test.go` and `TestRouterWaitsTheBackoff` (internal/router). Until it's done, `make test` (and CI) fails in `internal/router`, and an unavailable target is retried at once instead of after a backoff.
