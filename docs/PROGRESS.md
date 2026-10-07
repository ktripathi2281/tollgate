# Progress

**Current milestone:** M1, non-streaming through the mock. Complete; waiting for the go-ahead on M2.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0.
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24).
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes (`internal/provider`); OpenAI request validation, response types and error envelope (`internal/api`); the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases (first target only); `POST /v1/chat/completions` (non-streaming) and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap; scripts for the manual SDK and curl checks.

## M1 acceptance

| Check | Status |
|---|---|
| A stock OpenAI SDK gets a valid completion from `mock-fast` (manual) | passes: `scripts/openai_sdk_check.py` with openai 3.26.0, and `scripts/curl_check.sh` (2026-10-07) |
| Validation table tests cover every supported and unsupported field | passes |
| The in-flight cap sheds with 503 under a concurrency test | passes (`TestInflightCapShedsExcessRequests`) |

## Next

- Maintainer: review M1 and give the go-ahead for M2.
- M2: streaming.

## Open questions

- None.

## Pending exercises

- None. The M1 exercise (`parseStop`) is done.
