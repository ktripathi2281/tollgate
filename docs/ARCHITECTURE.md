# Architecture

Tollgate is one Go binary, `tollgate`, with subcommands: `serve` runs the gateway; `migrate`, `keys` and `reconcile` work on its database. It keeps its state in Postgres (keys, budgets, idempotency records, request logs) and in memory (rate-limit buckets, circuit breakers, the key cache), so it runs as a single instance.

## Packages

| Package | What it does |
|---|---|
| `cmd/tollgate` | The CLI: `serve` wires everything together and owns the background goroutines; `migrate`, `keys`, `reconcile`. |
| `cmd/mockupstream` | A standalone OpenAI-compatible mock provider, for tests, demos and benchmarks. |
| `cmd/loadgen` | The load generator behind `make bench`. |
| `internal/config` | Loads the YAML config strictly, applies defaults, and reports every problem at once. Prices are parsed to integer micro-USD here. |
| `internal/api` | The OpenAI wire format: request parsing and validation, response and stream-chunk types, the error envelope, the SSE writer. |
| `internal/server` | The HTTP server: middleware, the chat handler and its pipeline, streaming, per-request accounting (`money.go`), the request-log row and metrics (`observe.go`), `/readyz` and the admin handler, graceful shutdown. |
| `internal/auth` | Virtual keys: format, SHA-256 hashing, and the lookup cache. |
| `internal/limiter` | Token buckets for requests and tokens per minute, one pair per key. |
| `internal/budget` | Holds, settles, releases and the sweep of expired holds. |
| `internal/idempotency` | `Idempotency-Key`: claim, replay, conflict, takeover. |
| `internal/router` | Aliases and their targets, retries with backoff, fallback, circuit breakers, the commit point and stream watchdogs. |
| `internal/provider` | The provider interface, error classes, shared HTTP client; `openai`, `anthropic` and `mock` implement it. |
| `internal/sse` | Reads server-sent events from upstream streams. |
| `internal/store` | Postgres: the pgx pool, embedded goose migrations, sqlc queries (`store/db`, generated), and the transactions for holds and idempotency. `storetest` gives tests a schema each. |
| `internal/usage` | Token estimates, the input bound for holds, cost, and the request-log worker. |
| `internal/metrics` | The Prometheus registry and metrics. |
| `internal/money` | `Micros`, integer micro-USD, and parsing and formatting. |

Interfaces are declared where they are used. The server, for example, defines `Authenticator`, `Limiter`, `Budgets`, `Idempotency`, `RequestLog` and `Pinger`, and `serve` passes in the concrete types that satisfy them.

## A request's life

The middleware runs outside in (`internal/server/server.go`, `routes`):

1. **Shutdown tracking** counts handlers, so shutdown can wait for them.
2. **Request ID**: a fresh `req_...` in the context and the `X-Request-Id` header.
3. **Access log and metrics.** For a chat request, it also creates the request-log row (`requestInfo`) that the inner layers fill in.
4. **Panic recovery** turns a panic into a 500.
5. **In-flight cap**: a semaphore. With no slot free, the response is 503 at once.
6. **Authentication** resolves the bearer key through the cache: 401 if missing, unknown or revoked; 503 if the database can't be asked.
7. **Requests per minute**: 429 with `Retry-After`.

Then `handleChatCompletions` (`internal/server/handlers.go`) runs the pipeline of brief section 5:

8. **Read and validate** the body: size-limited, then field by field. The alias must exist and the key must be allowed to use it (404 otherwise). The output-token limit is the client's or the alias default, capped by the alias ceiling.
9. **Idempotency** (if the header is sent): claim the key, or replay the stored response, or answer 409 (still running) or 422 (another body).
10. **Tokens per minute**: reserve the estimated prompt plus the output limit (429).
11. **Budget hold** (if the key has a budget): hold the worst case across the alias's targets (402).
12. **Route and call** (`internal/router`): validate the request against every target first, then try each target in order, retrying with backoff while the error class allows it and skipping targets whose breaker is open.
13. **Persist, then respond.** `accounting.finish` settles the hold with the actual cost, or releases it, and stores the response under the idempotency key, or removes the record if the request failed. Only then is the response written.
14. **After the response:** the unused tokens are refunded, and the access log records the row and the metrics.

Each of steps 9 to 11 registers a deferred release as it takes something. Every exit, a panic included, unwinds them in reverse order; on the normal paths `finish` has already done the work, and the deferred calls find nothing left to do.

A streaming request differs after step 12:
- The router returns once the first content chunk has arrived. That is the commit point: before it, a failure is still an ordinary error response, and the router can retry or fall back.
- After it, the response is an event stream: one goroutine reads, translates and writes each chunk, so a slow client slows the upstream read rather than piling chunks up in memory. A failure after the commit point ends the stream with an error event and no `[DONE]`.
- When the stream completes, the outcome is saved, the assembled completion included, before the final `[DONE]`.

## Concurrency model

The goroutines, and who owns each:

| Goroutine | Started by | Ends when |
|---|---|---|
| `main` | the runtime | `run` returns. `signal.NotifyContext` cancels its context on the first SIGINT or SIGTERM; the second kills the process. |
| The public server's accept loop | `Server.Serve` (`internal/server/server.go`) | `http.Server.Shutdown` or `Close`; `Serve` waits for it before returning. |
| One per connection, running the handlers | `net/http` | The connection closes. `trackHandlers` counts the handlers, so shutdown can wait for them and then cancel what's left. |
| A watchdog per stream (`time.AfterFunc`) | the router (`internal/router/stream.go`) | It fires once, cancelling the stream's context with a cause, or is stopped when the stream ends. |
| The sweeper | `serve` | `serve` cancels the background context after the public server has stopped. |
| The request-log worker | `serve` | Same; it then drains its queue, and `serve` waits for it before closing the database. |
| The admin listener's server | `serve`, through `ServeAdmin` | Same, with 5s for a profile in progress. |
| pgx's pool maintenance | `pgxpool` | `Store.Close`, the last thing `serve` does. |

Shared state, and what guards it:

| State | Guard |
|---|---|
| Rate-limit buckets | One mutex in `limiter.Registry`, held for a few arithmetic steps. |
| The key cache | A mutex around the map, never held during the database lookup. |
| Each circuit breaker | Its own mutex. Half-open allows exactly one probe. |
| The in-flight cap | A buffered channel used as a semaphore. |
| Counters for dropped logs, settle failures and so on | `sync/atomic`, read by Prometheus at scrape time. |
| "Shutting down", for `/readyz` | `atomic.Bool`. |
| The request-log queue | A buffered channel: many senders, one receiver, sends never block. |
| A request's `accounting` and `requestInfo` | Not shared: each belongs to one request's goroutine. |

Money never depends on in-memory state. Holds, settles and idempotency claims are single SQL statements or transactions, and the database's row locks and unique keys decide races.

Shutdown (`Server.Serve`, `cmd/tollgate/serve.go`):
1. Stop accepting, and mark `/readyz` not ready.
2. Let in-flight requests, streams included, finish for up to `server.shutdown_grace`.
3. If any are still running, cancel their contexts with a "shutting down" cause. Streams end with an error event, and handlers have 5 seconds to unwind, which settles their holds.
4. Stop the admin listener and the sweeper, and let the request-log worker drain its queue.
5. Close the database.

## Failure modes

| Failure | What happens |
|---|---|
| An upstream returns 5xx, times out or refuses connections | Retried on the same target with backoff and full jitter, then the next target. Repeated failures open that target's breaker, so later requests skip it until a probe succeeds. If everything fails: 502, or 504 if time ran out. |
| An upstream rate-limits | A short `Retry-After` is waited out; a long one moves to the next target. If every target is rate-limited or open: 503 with `Retry-After`. |
| A provider's key is missing or wrong | Startup disables a provider without a key; a rejected key is a config error. Either way the alias falls back, logged at error level. |
| The client disconnects | Its context is cancelled; the upstream call stops within milliseconds; the hold is settled with what was used or released. A refused idempotent request's record is removed so the client can retry. |
| A stream stalls | The idle watchdog ends it after `timeouts.idle`; before the first token, `timeouts.first_token` applies, and the router can still fall back. |
| Postgres is down | Cached keys keep working until their entry expires, then 503 `auth_unavailable`. Budgeted and idempotent requests get 503. `/readyz` says not ready. Request-log rows queue, then drop and are counted. |
| Postgres is slow | Requests never wait on the request log. Budget and idempotency steps wait, within their own timeouts. |
| The process crashes mid-request | The hold stays held until the sweeper releases it after `budget.hold_ttl`. The idempotency record can be taken over after `idempotency.lock_for`. Startup checks that both outlast a request. |
| A settle fails after its retries | Logged at error level with the amounts and counted; the sweeper later releases the hold, so that spend isn't recorded. This window is a known limitation. |
| The gateway is overloaded | The in-flight cap sheds requests with 503 at once instead of queueing them. |

## Data model

Three migrations in `internal/store/migrations` build the schema:
- `api_keys`: keys, their limits, allowed aliases and budget;
- `budget_periods` and `budget_holds`: the money, with CHECK constraints that tie each hold's status to its columns;
- `idempotency_records`: stored responses, owned by the request that claimed them;
- `request_logs`: one row per chat request.

`tollgate reconcile` checks that each period's `held` and `spent` equal the sums of its holds.
