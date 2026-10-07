# Progress

**Current milestone:** M1, non-streaming through the mock. Built; waiting for the maintainer's review, the M1 exercise and the SDK check.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0.
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24).
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes (`internal/provider`); OpenAI request validation, response types and error envelope (`internal/api`); the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases (first target only); `POST /v1/chat/completions` (non-streaming) and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap; scripts for the manual SDK and curl checks.

## M1 acceptance

| Check | Status |
|---|---|
| A stock OpenAI SDK gets a valid completion from `mock-fast` (manual) | curl check passes; `scripts/openai_sdk_check.py` written, not yet run (needs `pip install openai`) |
| Validation table tests cover every supported and unsupported field | pass, except the `stop` cases, which wait for the exercise |
| The in-flight cap sheds with 503 under a concurrency test | passes (`TestInflightCapShedsExcessRequests`) |

## Next

- Maintainer: implement `parseStop`, run the SDK check, review M1.
- M2: streaming.

## Open questions

- None.

## Pending exercises

- **M1: `parseStop`** in `internal/api/stop.go`. Tests: `internal/api/stop_test.go` and `TestParseStopParam` in `internal/api/request_test.go`. Until it's implemented, `make test` (and so CI) fails in `internal/api`, and any request with a non-null `stop` gets a 400.
