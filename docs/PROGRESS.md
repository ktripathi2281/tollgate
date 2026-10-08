# Progress

**Current milestone:** M2, streaming. Complete; waiting for the go-ahead on M3.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0.
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24).
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes; OpenAI request validation, response types and error envelope; the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases (first target only); `POST /v1/chat/completions` (non-streaming) and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap. CI green.
- M2: streaming in the provider interface and the mock (in-process and over HTTP); a router that reads to the commit point and applies first-token, idle and total timeouts; SSE responses with OpenAI chunks, error events after the commit point and usage estimates; a write deadline per event; shutdown that drains streams and then cancels stragglers with a "shutting down" cause; timeouts in the config.

## M2 acceptance

All checks pass.

| Check | Test |
|---|---|
| Events are flushed one at a time | `TestStreamEventsArriveOneAtATime` (internal/server) |
| A client disconnect cancels the upstream call within 100 ms | `TestClientDisconnectCancelsUpstream` (internal/server) |
| First-token and idle timeouts fire | `TestStreamFailureBeforeCommitIsAnErrorResponse`, `TestStreamFailureAfterCommitIsAnErrorEvent` (internal/server); `TestStreamFirstTokenTimeout`, `TestStreamIdleTimeout` (internal/router) |
| Failure before the commit point gives an error response; after it, an SSE error event | the same two server tests |
| SIGTERM drains an in-flight stream; stragglers are cancelled after the grace period | `TestShutdownDrainsStream`, `TestShutdownCancelsStragglers` (internal/server); `TestServeDrainsStreamOnSIGTERM` (cmd/tollgate, real signal) |
| No goroutine leaks | `goleak` in every package that starts goroutines; `synctest` also fails any test that leaves goroutines in its bubble |

The manual check `scripts/openai_sdk_check.py` passed with openai 3.26.0 (2026-10-08): streaming, the usage chunk, and a mid-stream failure raised as `openai.APIError`.

## Next

- Maintainer: review M2 and give the go-ahead for M3.
- M3: real providers (OpenAI-compatible and Anthropic adapters), retries, fallback, circuit breakers. Needs model IDs and prices from the maintainer.

## Open questions

- None.

## Pending exercises

- None. The M2 exercise (`SSEWriter.WriteEvent`) is done.
