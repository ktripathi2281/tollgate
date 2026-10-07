# Tollgate: build brief

An OpenAI-compatible LLM gateway in Go with payments-grade spend control.

"Tollgate" is a working name. Keep it in as few places as possible (module path, binary name, key prefix, header prefix, README title) so a rename is one small commit.

## 1. What this is and why

Tollgate sits between applications and LLM providers. A client points any OpenAI SDK at Tollgate's base URL. Tollgate authenticates the request, enforces rate limits and a spend budget, routes the call to a provider with retries and fallback, streams the response back, and records what it cost.

This is a portfolio project with two jobs: show production-grade Go (concurrency, streaming, cancellation, graceful shutdown), and apply payment-system reliability patterns (idempotency keys, hold-and-settle, reconciliation) to LLM traffic. It is not trying to compete with existing gateways such as LiteLLM, Bifrost or GoModel. That means:

- Correctness, clarity, tests and measured performance matter more than feature count.
- Every non-obvious decision gets written down.
- The money path (section 9) is what makes this project different. Give it the most care.

## 2. How to work with me

I'm a backend engineer (Java and Spring Boot, payment flows: idempotency, retries, reconciliation). I'm new to Go, and I need to be able to explain every part of this code in an interview. So:

1. **One milestone at a time.** Stop at the end of each milestone (section 16) and wait for my go-ahead.
2. **At every stop, write `notes/milestone-N.md`** (`notes/` is gitignored) with: what you built, how to run it, the Go concepts it uses with file paths, where Go's approach differs from what I'd do in Java, and five interview-style questions about this milestone's code with short answers.
3. **Idiomatic and plain.** Small interfaces defined where they are used, explicit error handling with wrapped errors, `context.Context` as the first parameter, composition, no DI framework, no reflection, generics only where they clearly simplify. Mutexes for shared state, channels for handing off work. When two designs are close, pick the one that is easier to explain.
4. **Record decisions.** Add a short entry to `docs/DECISIONS.md` for every non-obvious choice: context, decision, alternatives rejected.
5. **Ask before adding a dependency** that isn't listed in section 13.
6. **Push back.** If something in this brief is wrong, ambiguous, outdated or over-engineered, say so and propose a fix before building. Provider APIs change: read the current API reference before writing each adapter (links in section 6) and don't rely on this brief or on memory for field names.
7. **Secrets.** I put provider API keys in `.env` myself. Don't ask me to paste them into the chat, and never print or commit them.
8. **Commits.** Small, one logical change each, conventional commit messages.

### Exercises (optional)

Unless I say "skip exercises", leave one small, self-contained function per milestone for me to write: add its tests, stub it with `// EXERCISE:` and a two-line hint, and list it in `docs/PROGRESS.md`. Good candidates: request validation (M1), the SSE event writer (M2), backoff with jitter (M3), the token bucket (M4), cost calculation (M5), batch flush logic (M6). Tell me which acceptance checks depend on the stub. The milestone is accepted once I've filled it in.

## 3. Scope

### In v1

- OpenAI-compatible chat completions, streaming and non-streaming, text only
- Three providers behind one interface: OpenAI-compatible, Anthropic, mock
- Model aliases with ordered fallback, retries, and a per-provider circuit breaker
- Virtual API keys with per-key rate limits (requests per minute and tokens per minute)
- Per-key budgets with hold-and-settle, idempotency keys, and a reconcile command
- Async request logs, Prometheus metrics, health checks, graceful shutdown
- Load generator, benchmarks, Docker, CI, deploy notes

### Out of v1 (don't build unless I ask)

- Tool or function calling, images, audio, embeddings, structured outputs, `n > 1`
- Dashboards or any UI, user management, SSO
- Caching, guardrails, PII redaction, prompt management
- Multi-instance deployment. v1 is a single instance; keep the limiter behind an interface so Redis could slot in later.
- Kubernetes manifests

## 4. Public API

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible. `stream: true` returns SSE. |
| `GET /v1/models` | Lists configured aliases in OpenAI list format. |
| `GET /healthz` | Liveness. |
| `GET /readyz` | Readiness: database reachable and not shutting down. |

`/metrics` and `net/http/pprof` are served on a separate admin listener, bound to localhost by default.

**Supported request fields:** `model` (an alias), `messages` (roles `system`, `developer`, `user`, `assistant`; content as a string or an array of text parts), `stream`, `stream_options.include_usage`, `max_tokens` or `max_completion_tokens`, `temperature`, `top_p`, `stop`.

**Unsupported fields:** reject with 400 any parameter that would change the output if it were silently dropped (`tools`, `tool_choice`, `response_format`, `n > 1`, `logprobs`, audio, non-text content parts). Ignore harmless ones (`user`, `metadata`). Propose the exact lists in `DECISIONS.md`.

**Responses:** OpenAI shapes (`chat.completion`, `chat.completion.chunk`, stream terminated by `data: [DONE]`). `id` is generated by Tollgate. `model` is the upstream model that actually served the request. Extra headers: `X-Request-Id`, `X-Tollgate-Provider`, `X-Tollgate-Attempts`, and `Idempotent-Replayed: true` on replays.

**Errors:** always the OpenAI error envelope, `{"error": {"message", "type", "code", "param"}}`. Mirror OpenAI's `type` and `code` strings where one exists.

| Case | Status |
|---|---|
| Missing or invalid key | 401 |
| Validation failure, unsupported field | 400 |
| Unknown alias, or alias not allowed for this key | 404 |
| Rate limited | 429 with `Retry-After` |
| Budget exhausted | 402 |
| Idempotency key reused with a different body | 422 |
| Idempotent request still in progress | 409 with `Retry-After` |
| Too many in-flight requests | 503 with `Retry-After` |
| All upstream targets failed | 502 |
| All upstream targets timed out | 504 |
| Every breaker open, or every target rate-limited upstream | 503 with `Retry-After` |

## 5. Request pipeline

Order matters. Each step either passes the request on or returns an error.

1. Panic recovery, request ID, access log
2. In-flight cap (a semaphore; shed with 503 when full)
3. Auth: resolve the virtual key
4. Requests-per-minute check
5. Parse and validate the body (size-limited)
6. Idempotency: begin, replay or reject (section 9.2)
7. Tokens-per-minute reservation
8. Budget hold (section 9.1)
9. Route and call the provider (sections 6 to 8)
10. Persist the outcome: settle or release the hold, complete the idempotency record
11. Write the response
12. Refund unused tokens-per-minute, enqueue the request log

**Persist before responding.** For non-streaming requests the outcome is saved (step 10) before the client sees it (step 11), as a payment API commits before it replies. If the process dies in between, the client retries with the same idempotency key and gets the stored response instead of a second upstream call. If settling fails here, the response is still sent, since the provider has already charged, and the failure is handled as in section 9.1. Streaming can't follow this order because bytes reach the client as they arrive, so steps 10 and 12 run when the stream ends.

**Unwind rule.** If anything fails after step 6, undo the earlier steps in reverse: release the hold, refund the token reservation, clear the idempotency record. A request must never leave a hold, a reservation or an `in_progress` record behind on any exit path, including panics and client disconnects. I expect this to be done with `defer` and tested per exit path.

## 6. Providers

Start from this shape. Change it if you find something more idiomatic, and record why.

```go
type Provider interface {
    Name() string
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    ChatStream(ctx context.Context, req *ChatRequest) (Stream, error)
}

type Stream interface {
    Next() (*Chunk, error) // io.EOF at normal end
    Close() error
}
```

The gateway has its own canonical request, response and chunk types. Adapters translate to and from each provider's wire format. No provider SDKs: use raw `net/http` so the wire formats stay visible. One shared `http.Client` per provider with explicit transport settings, never `http.DefaultClient`.

Every adapter maps upstream failures to these classes:

| Class | Examples | Router behaviour |
|---|---|---|
| Unavailable | 5xx, 529, connection errors, timeouts, error events inside a stream | Retry, then fall back. Counts toward the breaker. |
| Rate limited | 429 | Honour a short `Retry-After`, otherwise fall back. Doesn't trip the breaker. |
| Bad request | Other 4xx | Return to the client. No retry, no fallback. |
| Auth or config | 401, 403 from upstream | Fall back and log loudly. Never expose upstream detail to the client. |
| Cancelled | Client went away | Stop everything. Not a failure. |

### 6.1 OpenAI-compatible adapter

- Configurable `base_url`, so one adapter serves OpenAI and any other server that speaks the Chat Completions format.
- Always request usage on streams (`stream_options: {"include_usage": true}`), but forward the usage chunk to the client only if the client asked for it. Usage arrives in a final chunk whose `choices` array is empty. An interrupted stream may never deliver it.
- Decode tolerantly. Upstream chunks carry fields the gateway doesn't model (`obfuscation`, `service_tier`, `system_fingerprint`). Emit only canonical fields.
- Which token-limit parameter to send upstream (`max_completion_tokens` or `max_tokens`) is a per-provider config option, because compatible servers differ.
- Reference: https://developers.openai.com/api/reference/resources/chat and https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events

### 6.2 Anthropic adapter

This is the real translation work. As of writing:

- `system` and `developer` messages become the top-level `system` parameter. `stop` becomes `stop_sequences`. `max_tokens` is required.
- Stream events arrive in this order: `message_start` (carries input token usage), then for each content block `content_block_start`, one or more `content_block_delta` (text is in `delta.text` when `delta.type` is `text_delta`) and `content_block_stop`, then one or more `message_delta` (stop reason and cumulative output token usage), then `message_stop`.
- `ping` events can appear anywhere. Ignore them, but let them reset the idle timer. An `error` event can arrive mid-stream, for example `overloaded_error`. Unknown event types and non-text delta types must be ignored, not treated as errors.
- Map stop reasons deliberately (`end_turn` and `stop_sequence` to `stop`, `max_tokens` to `length`; check the docs for the rest) and usage (`input_tokens` and `output_tokens` to `prompt_tokens` and `completion_tokens`).
- Sampling parameters differ by provider and model (ranges, and which can be combined). Verify, then decide whether to clamp, drop or reject. Record it. Decide per alias, across all of its targets, including OpenAI reasoning models (section 7).
- Reference: https://platform.claude.com/docs/en/build-with-claude/streaming and https://platform.claude.com/docs/en/api/errors

### 6.3 Mock provider

Needed from M1. Two forms sharing one implementation:

- An in-process provider for unit tests.
- A standalone OpenAI-compatible HTTP server (`cmd/mockupstream`) for adapter integration tests and benchmarks.

Configurable: time to first token, interval between tokens, output token count, error rate, forced status codes, fail after N chunks, hang until timeout. Seedable randomness. It must honour context cancellation and give tests a way to assert that it saw the cancellation.

## 7. Routing and resilience

- A model alias maps to an ordered list of targets (provider plus upstream model).
- Validate a request against every target in its alias before the first attempt, so a fallback never turns a valid request into a 400. The budget hold already uses the most expensive target for the same reason.
- For each target: skip it if its breaker is open. Otherwise try up to `max_attempts_per_target` times, with exponential backoff and full jitter on Unavailable errors. Then move to the next target.
- One overall deadline covers all attempts.
- Circuit breaker per target (provider plus upstream model), because overload is often model-specific: closed, then open after N consecutive Unavailable failures, then half-open after a cool-down with a single probe, then closed on success. It must be correct under concurrent callers.
- If every target fails, return 502. Return 504 if the last failure was a timeout, and 503 with `Retry-After` if every breaker was open or every target was rate-limited upstream (with its own error `code`, so it doesn't read as the key's own rate limit). The log gets the full per-attempt story. The client gets a generic message.

## 8. Streaming rules

This is the core Go work. Get it exactly right.

1. **Commit point.** Write nothing to the client until the first content delta (or the end of the stream) has arrived from upstream. Before that point a failure may retry or fall back, including an early error event inside the stream. After it, no retry and no fallback.
2. **Failure after the commit point.** Send a final SSE event containing an OpenAI-style error object, end the stream, and record partial usage. Never truncate silently.
3. **Cancellation.** The upstream request uses a context derived from the client request. When the client disconnects, the upstream call is cancelled promptly. Test: the mock observes the cancellation within 100 ms.
4. **Timeouts.** Separate connect, first-token and idle (gap between chunks) timeouts, plus the overall deadline. No blanket `http.Client.Timeout` and no server `WriteTimeout` that would kill long streams. Use per-request deadlines. Do set `ReadHeaderTimeout` and a body size limit on the server.
5. **Flushing.** Flush after every event. Set `Content-Type: text/event-stream`, `Cache-Control: no-cache` and `X-Accel-Buffering: no`.
6. **Backpressure.** One read-translate-write loop per stream. No goroutine per chunk, and no unbounded buffering when the client is slow.
7. **Usage.** Take token counts from the provider. If they're missing (interrupted stream, or the provider didn't send them), estimate and mark the record `usage_estimated`.
8. **Shutdown.** On SIGTERM: fail readiness, stop accepting, let in-flight streams finish for up to `shutdown_grace`, then cancel what's left, flush the log worker, and close the pool.
9. **No goroutine leaks**, verified in tests.

## 9. The money path

Rules for everything in this section:

- Money is integer micro-USD (`int64` in Go, `BIGINT` in Postgres). No floats anywhere in cost, price or budget code. Parse prices from decimal strings. Round costs up.
- Prices come from config, in USD per million tokens, per provider and model. Every target must have a price or startup fails. The mock has a fake non-zero price so this path is exercised in tests and benchmarks.
- This path is synchronous and transactional. The request log (section 10) is asynchronous and may drop rows under overload. They are deliberately separate, and money never depends on the log.

### 9.1 Budgets: hold and settle

A key can have a budget: an amount plus a `daily` or `monthly` period, in UTC. The flow mirrors card authorisation and capture.

1. **Hold.** Before calling the provider, compute the worst-case cost: an upper bound on input tokens at the input price, plus the maximum output tokens at the output price, using the most expensive target in the alias. The input bound is the UTF-8 byte count of the messages plus a fixed per-message overhead, because a byte-level BPE tokenizer never produces more tokens than bytes. OpenAI's tokenizers are byte-level; Anthropic's isn't published, so for Anthropic this is a documented assumption. The ~4 characters per token estimate stays for the tokens-per-minute limit only. In one transaction, insert a hold row and increase the period's `held` amount only if `spent + held + hold <= budget`. If that condition fails, return 402.
2. **The upper bound must be real.** If the client omits a token limit, inject the alias's `default_max_tokens` upstream. Reject requests above the alias's `max_tokens_ceiling`.
3. **Settle.** After the response, in one transaction: mark the hold settled with the actual cost, decrease `held` by the hold amount, and increase `spent` by the actual cost.
4. **Release.** If no usage was incurred, mark the hold released and decrease `held`.
5. **Sweeper.** A background job releases holds older than `hold_ttl`, like an expired authorisation. This is the crash-recovery path. Startup fails unless `hold_ttl` is longer than `timeouts.total` plus `shutdown_grace`, so the sweeper never releases a hold whose request is still running.

Settle and release are idempotent: the state change is guarded by `WHERE status = 'held'`, so doing either twice is a no-op. If settling fails, retry a few times, then log at error level with the amounts and increment a metric. Document that window as a known limitation. A settle that finds its hold already released by the sweeper (a late settle) changes nothing, but it is logged at error level with the amount and counted in its own metric, because that spend was not recorded.

Settle always records the actual cost, even when it takes `spent` over the budget: the tokens were used, so they're owed.

A hold belongs to the period it was placed in, even if it settles after the period rolls over.

**Invariants.** Tests assert all four. `tollgate reconcile` checks 1 and 2 and exits non-zero on a mismatch.

1. `held` for a key and period equals the sum of its holds in status `held`.
2. `spent` equals the sum of `settled_micros` over its settled holds.
3. `spent + held` never exceeds the budget at the moment a hold is placed.
4. Every hold reaches exactly one terminal state, exactly once.

### 9.2 Idempotency keys

Follow the semantics of the IETF `Idempotency-Key` header draft (read it first). The key is optional, client-generated, at most 255 characters, and scoped to the API key.

- **First use.** Insert an `in_progress` record (the unique constraint decides races), process the request, store the response, mark it `completed`.
- **Completed, same request hash.** Replay the stored response with `Idempotent-Replayed: true`. No provider call, no hold, no token reservation. It still counts against requests per minute.
- **Same key, different request hash.** 422.
- **Still in progress.** 409 with `Retry-After`. An `in_progress` record past its `locked_until` is treated as abandoned and can be taken over. Startup fails unless the lock duration is longer than `timeouts.total`, so a takeover never races a request that is still running.
- **Failures.** Only successful responses are stored. If the request fails, remove the record so the client can retry with the same key.
- **Streaming.** Accumulate the text while streaming, store the assembled completion on success, and replay it as a minimal valid stream. If that turns out messy, propose rejecting `Idempotency-Key` on streaming requests instead and we'll decide together.
- **Retention.** Records expire after 24 hours and are swept in the background. Responses above a size cap are not stored.
- **Hash.** SHA-256 of the raw request body. Note the consequence (equivalent JSON serialised differently counts as different) in `DECISIONS.md`.

## 10. Keys, limits, logs, observability

**Virtual keys.** Format: `tg_` plus at least 32 random bytes, encoded. Store only the SHA-256 hash and a short display prefix, and record in `DECISIONS.md` why a fast hash is acceptable here. Show the full key once, at creation. Each key has a name, requests per minute, tokens per minute, an optional budget, an optional list of allowed aliases, and a revoked timestamp. Keys are managed with CLI subcommands (`tollgate keys create|list|revoke`), not an admin HTTP API. Cache key lookups in memory with a short TTL and document the revocation delay.

**Rate limits.** A hand-written token bucket with an injectable clock. One pair of buckets (requests, tokens) per key, in a registry that evicts idle entries. The token bucket reserves estimated input plus maximum output tokens at admission and refunds the unused part afterwards. Token estimation is a documented heuristic (about four characters per token); provider-reported usage is authoritative afterwards. The limiter sits behind an interface.

**Request logs.** One row per request: request ID, key, alias, provider and model used, stream flag, status, error class, attempt count, token counts, `usage_estimated`, cost, total latency, time to first token, replay flag. Handlers do a non-blocking send to a buffered channel. One worker batches by size or by time and bulk-inserts. If the channel is full, drop the row and count it. On shutdown, drain and flush.

**Metrics.** Prometheus, on the admin listener: request count and duration by route and status; gateway overhead (total time minus upstream time); time to first token; upstream requests by provider and outcome; retries; fallbacks; breaker state; rate-limit and budget rejections; tokens and cost by provider; idempotent replays; in-flight requests; dropped logs; settle failures. Never label by key or request ID.

**Logging.** `log/slog`, JSON output. Never log API keys, auth headers, prompts or completions.

## 11. Data model (Postgres)

These are the columns that matter. You write the DDL. Timestamps are `timestamptz`. A trailing `?` means nullable.

```
api_keys             id, name, key_hash (unique), key_prefix, rpm, tpm,
                     budget_micros?, budget_period?, allowed_models?,
                     created_at, revoked_at?

budget_periods       key_id, period_start, spent_micros, held_micros
                     PK (key_id, period_start); CHECK both amounts >= 0

budget_holds         id, key_id, period_start, request_id, amount_micros,
                     settled_micros?, status (held | settled | released),
                     created_at, expires_at, resolved_at?

idempotency_records  key_id, idem_key, request_hash,
                     status (in_progress | completed),
                     response_status?, response_body?,
                     locked_until, created_at, expires_at
                     PK (key_id, idem_key)

request_logs         the fields listed in section 10
```

## 12. Configuration

YAML file plus environment variables. Secrets (`DATABASE_URL`, provider keys) come only from the environment. Validate everything at startup and fail with a clear message. Field names below are a sketch; the semantics are what matter.

```yaml
server:
  addr: ":8080"
  admin_addr: "127.0.0.1:6060"
  shutdown_grace: 30s
  max_body_bytes: 2097152
  max_inflight: 2000

providers:
  openai:
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
  anthropic:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
  mock:
    type: mock
    ttft: 100ms
    token_interval: 10ms
    output_tokens: 50

models:
  fast:
    default_max_tokens: 1024
    max_tokens_ceiling: 4096
    targets:
      - { provider: openai, model: "ASK_ME" }
      - { provider: anthropic, model: "ASK_ME" }
  mock-fast:
    default_max_tokens: 256
    max_tokens_ceiling: 1024
    targets:
      - { provider: mock, model: mock-1 }

pricing:              # USD per 1M tokens, as decimal strings
  mock/mock-1: { input: "1.00", output: "2.00" }

retry:    { max_attempts_per_target: 2, base_backoff: 200ms, max_backoff: 2s }
timeouts: { connect: 5s, first_token: 30s, idle: 30s, total: 300s }
breaker:  { failure_threshold: 5, open_for: 30s }
budget:   { hold_ttl: 10m }
idempotency: { lock_for: 6m, retention: 24h, max_response_bytes: 1048576 }
```

Don't guess real model IDs or prices. Ask me at M3 and I'll supply them from the providers' current pages.

## 13. Stack

- **Go 1.26 or newer.** Use the latest stable version that's installed, and tell me if it's older. Module path: `github.com/ktripathi2281/tollgate`.
- Standard library first: `net/http` (method-and-path patterns on `ServeMux`, `http.ResponseController`), `log/slog`, `context`, `encoding/json`, `testing`, `net/http/httptest`, `testing/synctest`.
- `github.com/jackc/pgx/v5` with `pgxpool`. `sqlc` for typed queries, with generated code committed. `github.com/pressly/goose/v3` with embedded migrations.
- `github.com/prometheus/client_golang`.
- A maintained YAML library. Check its maintenance status before choosing.
- Test-only: `go.uber.org/goleak`.
- No web framework, ORM, DI container or provider SDK.
- Tooling: `gofmt`, `go vet`, `golangci-lint`; a `Makefile` with `build`, `test`, `lint`, `run`, `migrate`, `bench` and `smoke`; a multi-stage `Dockerfile` producing a static binary; `docker-compose.yml` with the gateway, Postgres, the mock upstream and Prometheus; GitHub Actions running lint, `go test -race ./...`, build, and the Postgres-backed tests against a service container.

## 14. Repo layout

```
cmd/tollgate/          serve, migrate, keys, reconcile
cmd/mockupstream/      standalone mock server
cmd/loadgen/           load generator
internal/api/          OpenAI-compatible types, validation, error envelope
internal/server/       HTTP server, middleware, handlers, SSE writer
internal/provider/     interface and error classes
internal/provider/openai/
internal/provider/anthropic/
internal/provider/mock/
internal/router/       aliases, retry, fallback, circuit breaker
internal/limiter/      token bucket, per-key registry
internal/budget/       hold, settle, release, sweeper, reconcile
internal/idempotency/
internal/usage/        cost calculation, async request-log worker
internal/store/        migrations, sqlc queries, pool
internal/config/
internal/metrics/
deploy/                compose files, Prometheus config
docs/                  BRIEF.md, PROGRESS.md, DECISIONS.md, ARCHITECTURE.md, BENCHMARKS.md
notes/                 gitignored: milestone summaries for me
```

## 15. Testing standards

- `go test -race ./...` passes at every stop. Tests are table-driven where that fits.
- No real network in unit tests. Adapters are tested against `httptest` servers replaying recorded fixtures.
- Anything involving time (backoff, breaker, limiter, timeouts, sweepers, batch flush) is tested with a fake clock or `testing/synctest`, never with real sleeps.
- `goleak` in every package that starts goroutines.
- Tests that need Postgres run against a real Postgres (compose locally, a service container in CI), behind a build tag or an environment variable.
- Real provider calls happen only in `make smoke`, which is skipped when keys are absent.
- Coverage percentage is not a goal. The acceptance checks below are.

## 16. Milestones

Each one ends in something runnable. Acceptance checks are automated tests unless marked manual.

### M0: Scaffold

Module, layout, Makefile, lint, CI, config loading with validation, `serve` with `/healthz`, structured logging, signal handling. Set up session continuity (section 20).

- `make build test lint` pass locally and in CI.
- Invalid config fails at startup with a clear message.
- SIGTERM exits cleanly.

### M1: Non-streaming through the mock

Canonical types, validation, error envelope, request ID, in-flight cap, mock provider in both forms, `POST /v1/chat/completions` without streaming, `GET /v1/models`. No auth yet; that arrives in M4.

- A stock OpenAI SDK pointed at Tollgate gets a valid completion from `mock-fast` (manual; add a script, plus a `curl` equivalent).
- Validation table tests cover every supported and unsupported field.
- The in-flight cap sheds with 503 under a concurrency test.

### M2: Streaming

SSE writer, stream loop, commit point, cancellation, timeouts, shutdown draining.

- Events are flushed one at a time (the test reads them as they arrive).
- A client disconnect cancels the upstream call within 100 ms.
- First-token and idle timeouts fire.
- A failure before the commit point gives a normal error response. A failure after it gives an SSE error event.
- SIGTERM drains an in-flight stream. After the grace period, stragglers are cancelled.
- No goroutine leaks.

### M3: Real providers and routing

OpenAI-compatible adapter, Anthropic adapter, error classification, retries, fallback, circuit breaker. Ask me for model IDs and prices here.

- Fixture tests for both adapters, streaming and non-streaming, covering usage, stop reasons, pings, unknown events, and an error in the middle of a stream.
- Router tests cover the matrix: retry then succeed, fall back, bad request not retried, rate limited with a short and a long `Retry-After`, cancelled, everything failed.
- Breaker tests: opens, half-opens, closes, and stays correct under concurrent callers.
- `make smoke` passes against the real APIs (manual).

### M4: Keys and limits

Postgres, migrations, sqlc, the key CLI, auth middleware with its cache, allowed aliases, requests-per-minute and tokens-per-minute limits.

- 401 without a valid key. A revoked key stops working within the cache TTL.
- With N goroutines hammering one key under a frozen clock, the number admitted is exactly what the bucket allows.
- Token reservations are refunded on success, failure and cancellation.

### M5: The money path

Cost calculation, budgets (hold, settle, release, sweeper), `reconcile`, idempotency.

- 200 concurrent requests against a budget that fits exactly K holds, with the mock held open until all 200 are admitted or rejected: exactly K succeed, the rest get 402, and all four invariants hold afterwards.
- Every exit path (success, upstream failure, failure after the hold, client disconnect, panic) leaves no hold in status `held`.
- A hold orphaned by a simulated crash is released by the sweeper.
- Double settle and settle-after-release are no-ops. A late settle after a sweeper release is logged and counted.
- Startup rejects a `hold_ttl` or idempotency lock that is too short for `timeouts.total`.
- A hold placed before a period boundary settles into its own period, and the new period starts at zero.
- Idempotency: replay, 422 on mismatch, 409 while in progress, retry allowed after a failure, takeover after `locked_until`, and N concurrent duplicates cause exactly one upstream call.
- `tollgate reconcile` passes after all of the above, and fails when a row is tampered with.

### M6: Logs and observability

Async request-log worker, metrics, admin listener, `/readyz`.

- The worker flushes by size and by time, drains on shutdown, and counts drops when the channel is full.
- Handlers never block on logging (test with a stalled database).
- Metrics are visible in Prometheus through compose (manual).

### M7: Benchmarks, docs, deploy

Sections 17 to 19.

## 17. Benchmarks

`cmd/loadgen` takes flags for URL, key, model, concurrency, duration, an optional target rate, and streaming on or off. It reports requests per second, error counts by status, and p50, p95 and p99 for total latency and time to first token.

Method:

1. Run `mockupstream` as a separate process with fixed latency.
2. Measure loadgen to the mock directly (the baseline), then loadgen through Tollgate to the mock. Overhead is the difference at p50, p95 and p99.
3. Scenarios: streaming and non-streaming; a key without a budget and a key with one (this shows what the two transactions cost); concurrency of 50, 200 and 1,000.
4. One run against a zero-latency mock to find the throughput ceiling. Take a CPU profile with pprof, name the top hotspots, fix one if it's worthwhile, and show before and after.
5. Run `tollgate reconcile` after the budgeted scenarios. It must pass.

`docs/BENCHMARKS.md` records the numbers, the machine, the Go version, `GOMAXPROCS`, where Postgres ran, and the exact commands. Say plainly that the upstream is a mock on loopback. No comparisons with other gateways unless they were run under the same conditions.

## 18. Documentation

- `README.md`: what it is, a request pipeline diagram (Mermaid), a quickstart (`docker compose up`, create a key, a `curl` call, an OpenAI SDK snippet using `base_url`), features, the money path explained with a sequence diagram, a benchmark summary, limitations, roadmap.
- `docs/ARCHITECTURE.md`: packages, request lifecycle, the concurrency model (which goroutines exist and who owns them), failure modes.
- `docs/DECISIONS.md` and `docs/BENCHMARKS.md`.
- Tone: factual. No claims that the tests or benchmarks don't support.
- The limitations section hides nothing: single instance, heuristic token estimates, prompt-cache discounts not modelled, the settle-failure window, idempotent responses stored for 24 hours.
- Write about the system, not about me. Don't describe the project as solo-built, built alone or anything similar, anywhere in the repo.

## 19. Deployment

- Local: `docker compose up` brings everything up, and the mock alias works without any provider keys.
- Hosted demo: a container host that supports long-lived HTTP responses, plus Supabase Postgres through `DATABASE_URL`. I'll choose the host at M7. Write `docs/DEPLOY.md` for the one I pick.
- Check Supabase's current connection docs before wiring pgx. Its transaction-mode pooler doesn't support the prepared statements pgx uses by default, so use a direct or session-mode connection, or change pgx's query exec mode.
- The public demo key is restricted to mock aliases with tight limits. Real provider keys stay in the host's secret store.

## 20. Session continuity

You start every session with a fresh context, so in M0:

- Keep this brief in the repo as `docs/BRIEF.md`.
- Create `CLAUDE.md` in the repo root, under about 60 lines: a one-paragraph project summary, the make targets, the rules from section 2 in short form, an import of `docs/PROGRESS.md`, and a sentence telling you to read the current milestone's sections of `docs/BRIEF.md` before coding. Mention the brief by path only. Don't import it; it's too long to load every session.
- Keep `docs/PROGRESS.md` short: current milestone, done, next, open questions, pending exercises. Update it at every stop.

## 21. Stretch (only after M7, and only if I ask)

- Redis-backed limiter for multiple instances
- Attach duplicate in-flight idempotent requests to the original instead of returning 409
- Finish idempotent requests in the background when the client disconnects
- Reconciliation against provider-reported usage
- Response cache, tool calling, embeddings, OpenTelemetry tracing, a read-only usage page

## 22. Definition of done

- Every acceptance check passes with `-race`, with no goroutine leaks.
- `make bench` reproduces `docs/BENCHMARKS.md`, and `tollgate reconcile` passes after it.
- The README quickstart works from a clean clone.
- No secrets in the repo and nothing sensitive in logs.
- I can answer the interview questions from every milestone without looking.

## 23. Start here

1. Read this whole brief.
2. Check the toolchain (`go version`, Docker, `sqlc`, `golangci-lint`) and tell me what's missing.
3. Tell me anything you'd change in this brief, and why, before writing code.
4. Then start M0.

## 24. Revisions

Changes agreed after the first review (2026-10-07). Each has an entry in `docs/DECISIONS.md`.

1. Non-streaming requests persist the outcome (settle, complete the idempotency record) before writing the response (section 5).
2. Startup checks that `hold_ttl` and the idempotency lock outlast a request, and late settles are logged and counted (sections 9.1, 9.2, 12).
3. Budget holds bound input tokens by UTF-8 byte count, not the ~4 characters per token estimate (section 9.1).
4. Requests are validated against every target in the alias (sections 6.2, 7).
5. One circuit breaker per target (provider plus model), not per provider (section 7).
6. 503 with `Retry-After` when every breaker is open or every target is rate-limited upstream (sections 4, 7).
