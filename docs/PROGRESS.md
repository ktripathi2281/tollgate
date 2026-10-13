# Progress

**Current milestone:** M7, benchmarks, docs and deployment. Built; waiting for the M7 exercise, the maintainer's review, and the hosted demo's deploy, which needs the maintainer's own Render and Supabase accounts.

## Done

- Toolchain in WSL Ubuntu: Go 1.27.1, sqlc 1.31.1, golangci-lint 2.14.0. Postgres 18 in Docker (Docker Desktop with WSL integration).
- Brief reviewed. Six changes agreed and applied to `docs/BRIEF.md` (section 24), and a seventh during M3.
- M0: module and layout, config loading with strict validation, `tollgate serve` with `/healthz`, JSON logging, graceful shutdown on SIGINT and SIGTERM, Makefile, golangci-lint config, GitHub Actions workflow, session continuity files. CI green.
- M1: config for providers, model aliases and prices; canonical chat types and upstream error classes; OpenAI request validation, response types and error envelope; the mock provider in-process and as `cmd/mockupstream`; a router that resolves aliases; `POST /v1/chat/completions` and `GET /v1/models`; request IDs, access log, panic recovery, in-flight cap. CI green.
- M2: streaming end to end: the commit point, first-token, idle and total timeouts, SSE with error events after the commit point, usage estimates, a write deadline per event, and shutdown that drains streams then cancels stragglers. CI green.
- M3: the OpenAI-compatible and Anthropic adapters on raw net/http, with fixture tests; an SSE reader; error classification; per-target `sampling: false`; retries with backoff and full jitter, fallback, and a circuit breaker per target; 502/503/504 by outcome; the per-attempt story in the logs; providers without a key disabled; `make smoke`; the `fast` alias on Claude Haiku 5.5, and long-context price tiers. The manual smoke run waits for `ANTHROPIC_API_KEY` in `.env`.
- M4: Postgres through pgx, sqlc and embedded goose migrations; `tollgate migrate`; virtual keys with `tollgate keys create|list|revoke`; bearer auth with a 30s lookup cache; allowed aliases; per-key token buckets for requests and tokens per minute; Postgres-backed tests in per-test schemas, in CI against a service container. CI green.
- M5: cost in integer micro-USD, rounded up; per-key daily or monthly budgets with hold, settle, release and a sweeper; `Idempotency-Key` with replay, 409, 422, takeover and retry after failure; the outcome saved before responding; every exit path unwound by `defer`; `tollgate reconcile`; startup checks on `hold_ttl` and the idempotency lock. CI green.
- M6: a request-log row per chat request, written in batches with COPY and drained on shutdown; Prometheus metrics with bounded labels; an admin listener for `/metrics` and pprof; `/readyz`; a Dockerfile, and compose with Postgres, migrations, the gateway and Prometheus. CI green.
- M7: `cmd/loadgen`; `make bench` (`scripts/bench.sh`, `scripts/benchreport`), run and written up in `docs/BENCHMARKS.md`; holds and settles made single statements after the benchmark found a per-key lock bottleneck; `README.md`, `docs/ARCHITECTURE.md`, `docs/DEPLOY.md`; the hosted demo set up for Render's and Supabase's free tiers (`render.yaml`, `deploy/demo.yaml`); the mock upstream in compose.

## M7 checks (brief sections 17 to 19, 22)

| Check | Status |
|---|---|
| `cmd/loadgen` with URL, key, model, concurrency, duration, target rate and streaming; reports requests/s, errors by status, p50/p95/p99 for latency and time to first token | built; its percentiles wait for the exercise (`TestPercentiles`, `TestLoadNonStreaming`, `TestLoadStreaming`, `TestReport`, cmd/loadgen) |
| Baseline and through-gateway runs, streaming and not, with and without a budget, at 50, 200 and 1,000 workers; a zero-latency ceiling with a CPU profile and one fix, before and after | done, 2026-10-13: `docs/BENCHMARKS.md` |
| `tollgate reconcile` passes after the budgeted runs | pass: 0 mismatched, in every run |
| `make bench` reproduces `docs/BENCHMARKS.md` | yes, once the exercise is in |
| The README quickstart works from a clean clone | pass, 2026-10-13: a fresh clone without `.env` came up with `docker compose up -d --build`; every README command worked once the image's working directory was fixed |
| No secrets in the repository | pass: no keys tracked; the only password is the local Postgres container's |
| `docker compose up` brings everything up, and the mock alias works without provider keys | pass |
| Hosted demo | ready to deploy: `docs/DEPLOY.md` (Render free plus Supabase free); the deploy itself needs the maintainer's accounts |

## Next

- Maintainer: implement `percentilesOf`; review M7; deploy the demo by following `docs/DEPLOY.md`; and, whenever convenient, put `ANTHROPIC_API_KEY` in `.env` and run `make smoke` (still pending from M3).

## Open questions

- `fast` has one target, Claude Haiku 5.5, so it has nowhere to fall back to. An OpenAI target can be added once a model and its prices are chosen.

## Pending exercises

- **M7: `percentilesOf`** in `cmd/loadgen/main.go`. Tests: `TestPercentiles`, `TestLoadNonStreaming`, `TestLoadStreaming` and `TestReport` (cmd/loadgen). Until it's done, every percentile the load generator reports is 0, so `make bench` can't reproduce the tables, and `make test` (and CI) fails there.
