# Progress

**Current milestone:** M0, scaffold. Built; waiting for the maintainer's review and the M0 exercise.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0.
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24).
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files.

## M0 acceptance

| Check | Status |
|---|---|
| `make build test lint` pass locally | build and lint pass; test passes except the exercise's tests |
| `make build test lint` pass in CI | not pushed yet |
| Invalid config fails at startup with a clear message | passes (`TestServeFailsOnInvalidConfig`) |
| SIGTERM exits cleanly | passes (`TestServeExitsCleanlyOnSIGTERM`) |

## Next

- Maintainer: implement `ParseUSD`, then approve pushing to GitHub.
- M1: non-streaming chat completions through the mock provider.

## Open questions

- None.

## Pending exercises

- **M0: `money.ParseUSD`** in `internal/money/money.go`. The tests are in `internal/money/money_test.go`. Until it's implemented, `make test` (and so CI) fails in `internal/money` only. M1 uses it to parse prices from the config.
