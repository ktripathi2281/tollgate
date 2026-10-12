# Decisions

Short records of non-obvious choices: the context, the decision, and the alternatives rejected. Entries are numbered in the order they were made and are never renumbered.

## 1. Develop in WSL2, not native Windows (M0)

**Context.** The development machine runs Windows. CI, the Docker image and the hosted demo all run Linux.

**Decision.** Go and all tooling run inside WSL2 Ubuntu. The repo stays on the Windows drive and is reached from WSL as `/mnt/d/Projects/tollgate`. `.gitattributes` forces LF line endings so files edited on Windows and built on Linux have the same bytes.

**Rejected.** Native Windows: `go test -race` needs cgo plus a MinGW C compiler there, and Windows can't send SIGTERM to a process, so the shutdown acceptance tests couldn't run as written. Moving the repo into the WSL filesystem: builds would be a little faster, but Windows-side tools would have to reach it through `\\wsl$`.

## 2. Persist the outcome before writing a non-streaming response (M0, applies from M5)

**Context.** The original pipeline wrote the response, then settled the hold and completed the idempotency record. A crash between the two leaves the client with an answer and the record `in_progress`. After `locked_until`, a retry with the same key takes the record over and calls the provider again, so the request is paid for twice.

**Decision.** For non-streaming requests, settle and complete the idempotency record first, then write the response. A settle failure doesn't block the response, because the provider has already charged. Streaming keeps the old order, because its bytes are already on the wire.

**Rejected.** Writing first and accepting the window: that is the double charge idempotency keys exist to prevent. The cost of the chosen order is a few milliseconds of database time before the client sees the response; the benchmarks will measure it.

## 3. Startup checks for timing settings, and counted late settles (M0, applies from M5)

**Context.** If `hold_ttl` is shorter than a request can run, the sweeper releases a live hold. The later settle is guarded by `WHERE status = 'held'`, so it does nothing and the spend is never recorded. If the idempotency lock is shorter than `timeouts.total`, a duplicate can take over a request that is still running.

**Decision.** Startup fails unless `hold_ttl > timeouts.total + shutdown_grace` and the idempotency lock duration is longer than `timeouts.total`. A settle that finds its hold already released logs the amount at error level and increments its own metric.

**Rejected.** Letting a late settle move a released hold to settled: a hold would then reach two terminal states, which breaks invariant 4.

## 4. Budget holds bound input tokens by UTF-8 byte count (M0, applies from M5)

**Context.** Invariant 3 (`spent + held <= budget`) only holds when the hold is placed. If the hold underestimates the real cost, `spent` passes the budget at settle. The ~4 characters per token estimate badly undercounts some text: in Chinese or Japanese one character is often a whole token or more.

**Decision.** The input part of a hold is the UTF-8 byte count of the messages plus a fixed per-message overhead. A byte-level BPE tokenizer never produces more tokens than bytes, so this is a real upper bound. OpenAI's tokenizers are byte-level. Anthropic's isn't published, so for Anthropic this is a documented assumption. The tokens-per-minute limit keeps the cheaper estimate, because an error there only shifts admission timing and is refunded. Settle always records the actual cost, even above the budget.

**Rejected.** The estimate with a safety margin: there is no margin that covers every language. The byte bound over-holds English by about four times on the input side, which only matters close to the end of a budget, the way a fuel pump authorises more than the fill will cost.

## 5. Validate a request against every target in its alias (M0, applies from M3)

**Context.** Targets in one alias accept different parameters. For example, Anthropic's temperature range is narrower than OpenAI's, and OpenAI reasoning models restrict sampling parameters. If validation only considers the first target, a fallback can turn a valid request into an upstream 400.

**Decision.** The clamp, drop or reject decision is made per alias, across all of its targets, before the first attempt. The budget hold already uses the most expensive target for the same reason.

**Rejected.** Validating against the first target only, and treating an upstream 400 after a fallback as the client's fault.

## 6. One circuit breaker per target, not per provider (M0, applies from M3)

**Context.** Overload errors such as Anthropic's 529 are often specific to one model. A breaker for the whole provider would also block that provider's healthy models.

**Decision.** Breakers are keyed by provider plus upstream model. The cardinality is bounded by the config, so it's safe as a metric label.

**Rejected.** One breaker per provider: simpler to describe, but it fails over healthy models too. The cost of the chosen design is that a provider-wide outage, such as DNS, trips each model's breaker separately.

## 7. 503 when every target is unavailable for capacity reasons (M0, applies from M3)

**Context.** The brief returned 503 when every breaker was open, but didn't list that in the error table, and didn't cover every target being rate-limited upstream.

**Decision.** Both cases return 503 with `Retry-After` and a distinct error `code`.

**Rejected.** 429: clients would read it as the key's own rate limit, and OpenAI SDKs retry it as if it were.

## 8. YAML library: go.yaml.in/yaml/v3 (M0)

**Context.** The brief asks for a maintained YAML library. The long-standard `gopkg.in/yaml.v3` was archived in April 2025.

**Decision.** `go.yaml.in/yaml/v3`, the same code now maintained by the YAML organisation. Its v3 API is frozen and receives security fixes. It decodes durations such as `30s` into `time.Duration`, and `KnownFields(true)` turns unknown keys into errors with line numbers.

**Rejected.** `go.yaml.in/yaml/v4`: still a release candidate. `github.com/goccy/go-yaml`: maintained and has good error messages, but a frozen, widely used API is the safer choice for a config loader that needs nothing new.

## 9. Config: strict decoding, defaults, every problem reported at once (M0)

**Context.** The brief asks for startup validation with clear messages.

**Decision.** The file is decoded on top of `config.Default()`, so it only needs the fields that differ. Unknown keys and a second YAML document are errors. Validation collects every problem into one `ValidationError`, so a broken file is fixed in one pass. The config grows one section per milestone, as the code that uses each section arrives, so no field exists before something reads it.

**Rejected.** Writing the whole section 12 schema in M0: it would add fields with no code behind them and validation that couldn't be tested end to end. Returning the first error only: it makes fixing a config a restart loop. Overriding non-secret fields from environment variables: nothing needs it yet, and secrets already come only from the environment.

## 10. A small `internal/money` package (M0)

**Context.** Section 14 places cost calculation in `internal/usage`, next to the async request-log worker. Config loading (from M1) has to parse prices, and so does budget code (M5).

**Decision.** A separate `internal/money` package holds the `Micros` type (integer micro-USD), price parsing, and later cost calculation. It has no dependencies, so config, budget and usage can all import it. A named type, rather than a bare `int64`, stops token counts and amounts being mixed up by accident.

**Rejected.** Cost code in `internal/usage`: config would then import the package that owns the log worker and its database dependencies.

## 11. Shutdown and signal handling (M0)

**Context.** The brief asks for a clean exit on SIGTERM, a drain of in-flight streams for up to `shutdown_grace`, and no server `WriteTimeout`, because one would cut off long streams.

**Decision.** `main` turns SIGINT and SIGTERM into a cancelled `context.Context` with `signal.NotifyContext`, and passes it down. `server.Serve` reacts to the cancelled context by calling `http.Server.Shutdown`, which stops accepting connections and waits for in-flight requests, with a fresh timeout of `shutdown_grace`. If the grace period runs out, `Close` drops the remaining connections, and `Serve` returns an error, so the exit status is non-zero. After the first signal, default signal handling is restored (`context.AfterFunc(ctx, stop)`), so a second Ctrl-C kills the process at once. `ReadHeaderTimeout` is set and `WriteTimeout` is deliberately not.

**Rejected.** A goroutine that calls `os.Exit` from a signal handler: it skips the drain and every deferred cleanup. Treating an expired grace period as a clean exit: it hides requests that were cut off.

M2 extends this: request contexts will derive from a base context that is cancelled when the grace period ends, so handlers see the cancellation and unwind holds and reservations before the process exits.

## 12. End-to-end process tests re-run the test binary (M0)

**Context.** "SIGTERM exits cleanly" and "invalid config fails at startup" are claims about a real process: its signals, exit code and stderr. Calling `run` in-process can't test either.

**Decision.** `TestMain` in `cmd/tollgate` checks an environment variable. When it is set, the test binary runs the real `main` instead of the tests. Tests start `os.Args[0]` as a child process with that variable set, read its JSON logs from stdout, send it SIGTERM, and check the exit status. The SIGTERM test is skipped on Windows, which can't deliver SIGTERM to another process.

**Rejected.** Running `go build` inside the test: slower, and it depends on the toolchain being on `PATH` at test time. Sending the signal to the test process itself: it would test signal delivery, but not the exit status or the shutdown logs.

## 13. Which request parameters are supported, ignored or rejected (M1)

**Context.** The brief asks for a 400 on any parameter that would change the output if silently dropped, and for harmless ones to be ignored. OpenAI's chat completions API reference (checked 2026-10-07) lists 37 request parameters.

**Decision.** Every one of the 37 is in exactly one group, and a test checks the groups against the list:

- **Supported (9):** `model`, `messages`, `stream`, `stream_options`, `max_tokens`, `max_completion_tokens`, `temperature`, `top_p`, `stop`.
- **Ignored (10):** `user`, `metadata`, `store`, `safety_identifier`, `prompt_cache_key`, `prompt_cache_options`, `prompt_cache_retention`, `service_tier`, `parallel_tool_calls`, `prediction`. They affect storage, caching, billing tier, latency or abuse tracking, not the generated text.
- **Rejected unless null or a no-op value (18):** `n` (1), `logprobs` (false), `top_logprobs` (0), `presence_penalty` (0), `frequency_penalty` (0), `logit_bias` ({}), `tools` ([]), `functions` ([]), `tool_choice` ("none"), `function_call` ("none"), `response_format` ({"type": "text"}), `modalities` (["text"]), and, accepted only as null, `seed`, `audio`, `reasoning_effort`, `verbosity`, `web_search_options`, `moderation`.

Any other top-level parameter is rejected as unknown. Numbers compare by value, so `0.0` counts as `0`; other no-op values compare as compact JSON. JSON `null` always means "not set", as it does in OpenAI's API.

Inside messages, roles `system`, `developer`, `user` and `assistant` are supported, and `tool` and `function` are rejected (no tool calling). Content is a string or an array of `text` parts; `image_url`, `input_audio`, `file` and `refusal` parts are rejected. `name`, `tool_calls`, `tool_call_id`, `function_call`, `refusal` and `audio` are rejected unless null or empty. In `stream_options`, `include_usage` is supported and `include_obfuscation` is ignored.

**Rejected.** Rejecting a restricted parameter whatever its value: clients and frameworks often send defaults such as `n: 1` or `presence_penalty: 0`, and those change nothing. Ignoring unknown parameters: a new OpenAI parameter could change the output, so it is safer to fail loudly until it has been classified. Accepting `seed` and dropping it: the client would believe the output is reproducible when it isn't.

## 14. Error codes and status mapping (M1)

**Context.** The brief asks for OpenAI's error envelope, with OpenAI's `type` and `code` strings where one exists.

**Decision.** Types are `invalid_request_error` for 4xx and `server_error` for 5xx. Codes reuse OpenAI's where they match (`model_not_found`, `unsupported_parameter`, `unsupported_value`, `invalid_type`, `invalid_value`, `missing_required_parameter`) and follow the same style otherwise (`invalid_json`, `unknown_parameter`, `request_too_large`, `overloaded`, `upstream_error`, `upstream_timeout`, `upstream_rate_limited`, `internal_error`). Upstream failures map by class: bad request 400 (with the upstream message, since it concerns the client's input), rate limited 503 with `Retry-After`, auth or config 502, timeout 504, anything else 502. Only bad requests carry upstream detail; the rest go to the log. A body over `max_body_bytes` gets 413, which the brief's table didn't list. Any other `/v1/` path gets OpenAI's 404, `Invalid URL (METHOD /path)`.

**Rejected.** Passing upstream error bodies through: they can include account details from the gateway's own provider key.

## 15. Request IDs are always generated by the gateway (M1)

**Context.** Every response carries `X-Request-Id`, and the ID will key logs and budget holds.

**Decision.** Each request gets `req_` plus 26 random base32 characters from `crypto/rand.Text`. An incoming `X-Request-Id` is ignored.

**Rejected.** Honouring a client's ID: two clients could send the same one, and a client-chosen string would end up in logs and database rows.

## 16. Token limits: alias default and ceiling (M1)

**Context.** The budget hold (M5) needs a real upper bound on output tokens.

**Decision.** `max_tokens` and `max_completion_tokens` are both accepted as the same limit, but not both in one request. If the client sends neither, the alias's `default_max_tokens` is sent upstream. A limit above `max_tokens_ceiling` gets a 400. Canonical messages keep text parts separate (`Parts []string`), so adapters can map them one to one instead of guessing how to join them.

**Rejected.** Clamping a limit that is over the ceiling: the client would get a shorter reply than it asked for without being told.

## 17. In-flight cap (M1)

**Decision.** A buffered channel of size `max_inflight` is the semaphore. A request takes a slot without blocking or is shed at once with 503, `Retry-After: 1` and code `overloaded`. The cap covers `/v1/` only, so health checks still answer when the gateway is saturated.

**Rejected.** Queueing for a slot: under overload a queue only adds latency, and the client's own retry is a better queue. `golang.org/x/sync/semaphore`: a weighted semaphore isn't needed, and it would be a new dependency.

## 18. The mock: one implementation in two forms (M1)

**Decision.** `mock.Provider` implements the provider interface in-process, and `mock.Handler` wraps it in an OpenAI-compatible HTTP API, served by `cmd/mockupstream`. The handler parses requests leniently, as a real provider would. Randomness comes from a seeded PCG source behind a mutex. Tests see requests arrive and cancellations land through two optional hooks, `OnCall` and `OnCancel`.

**Rejected.** Separate implementations for the two forms: they would drift. A channel of events instead of hooks: tests would have to drain it, or the mock would block.

## 19. Smaller M1 choices (M1)

- **Streaming requests get a 400** until M2, rather than silently returning a non-streaming response.
- **`Content-Type` isn't checked.** The body is parsed as JSON whatever the header says, so a plain `curl -d` works.
- **The router calls only an alias's first target** in M1. Retries, fallback and breakers arrive in M3. `Router.Chat` takes the request by value, so each target gets its own copy with its own upstream model ID.
- **Validation returns `error`, not `*api.Error`.** A function returning a nil `*Error` through an `error` interface produces a non-nil interface value. Returning `error` and unwrapping with `errors.AsType` avoids that trap.
- **The `Provider` interface lives in `internal/provider`,** not with the router that consumes it. It is the contract every adapter implements, the way `io.Reader` lives in `io`.
- **Pricing arrives with targets in M1,** because "every target has a price" is a startup check on targets. Prices are first used for cost in M5. Decision 9's rule ("no field exists before something reads it") still holds, with validation as the reader.

## 20. The commit point lives in the router (M2)

**Context.** Brief section 8: write nothing until the first content arrives; before that, a failure may still retry or fall back (M3); after it, it can't.

**Decision.** `router.ChatStream` opens the provider stream and reads up to the commit point: the first chunk with content or a finish reason, or the end of the stream. A failure before that is returned as an ordinary error, and the server answers with an ordinary JSON error response. After that, the caller gets a committed `router.Stream` and writes the response headers. Keepalive chunks (a chunk with no fields set, such as an Anthropic ping) are dropped before the commit point; after it, `Stream.Next` skips them but uses them to reset the idle timeout. A usage chunk that arrives before any content is kept, not dropped. `provider.Stream` follows the brief's shape (`Next` returning `io.EOF`, plus `Close`), used from one goroutine. A stream keeps the context it was opened with, the way an `http.Response` body does; that is the usual exception to "don't store contexts in structs".

**Rejected.** Committing on the first chunk of any kind: OpenAI's first chunk is an empty role delta, so the gateway would commit before knowing the upstream can produce content, and lose the chance to fall back. Range-over-func iterators (`iter.Seq2`) instead of `Next`/`Close`: they make the end-of-stream cleanup implicit, which is harder to explain and to test.

## 21. Timeouts are a watchdog timer that cancels the stream's context (M2)

**Context.** Separate first-token, idle and total timeouts, without a goroutine per chunk, and without a blanket `http.Client.Timeout` or server `WriteTimeout`.

**Decision.** Each stream has one `time.AfterFunc` timer, the watchdog. Until the commit point it enforces `first_token`, counted from the start of the call, so it includes opening the stream. Keepalives don't extend it, because they don't show the model is producing anything. At the commit point it is replaced by an `idle` timer, which every chunk, keepalives included, resets. When a watchdog fires, it cancels the stream's context with a cause (`context.WithCancelCause`): `ErrFirstTokenTimeout` or `ErrIdleTimeout`. `total` is a `context.WithTimeoutCause` covering the whole call, attempts and stream included. The provider only sees its context end; the router reads the cause and returns it. Every timeout error wraps `context.DeadlineExceeded`, so the server maps all of them to 504 `upstream_timeout`, and `provider.ClassOf` treats them as Unavailable. A non-streaming call gets the total deadline only, because it has no first token to watch. The connect timeout arrives with the HTTP adapters in M3, as a transport setting.

**Rejected.** Running `Next` in a goroutine and racing it against a timer: a goroutine per chunk, which the brief rules out, and the abandoned read can't be stopped. Resetting the same timer at the commit point: `Reset` reschedules the original function, which reported every timeout as a first-token timeout. A test caught this.

## 22. What a client sees when a stream fails (M2)

**Decision.** If the stream fails after the commit point, the gateway sends one last event in OpenAI's error shape, `data: {"error": {...}}`, and ends the stream without `data: [DONE]`. The OpenAI Python SDK raises `APIError` when it sees an event with an `error` key; this was checked with openai 3.26.0 against the `mock-flaky` alias. The finish chunk is sent once the upstream ends; a stream that ends without a reason gets `stop`. The usage chunk (empty `choices`, the totals) is sent only when the client asked with `stream_options.include_usage`. Other chunks omit `usage` rather than sending `null`; SDKs treat the two the same. When the provider reports no usage, the gateway estimates it at about four characters per token, and a `stream ended` log line records the outcome and `usage_estimated`. M5 and M6 store this instead of only logging it.

**Rejected.** Sending `[DONE]` after the error event: a client that only waits for `[DONE]` would take a partial reply for a complete one, which is the silent truncation the brief forbids.

## 23. Shutdown: a base context with a "shutting down" cause (M2)

**Context.** On SIGTERM, in-flight streams get up to `shutdown_grace` to finish, and whatever is left is then cancelled. `http.Server.Shutdown` waits for active connections but never cancels them, and `Close` drops connections without telling the handlers why.

**Decision.** Every request context derives from a base context set through `http.Server.BaseContext`. If the grace period runs out, `Serve` cancels the base context with the cause `errShuttingDown`. Each handler still running sees its context end, recognises the cause, and sends a final error event with code `shutting_down` (a 503 with `Retry-After` if it hasn't committed yet). A `sync.WaitGroup` counts running handlers. `Serve` waits for them for at most `unwindTimeout` (5s), then closes the remaining connections. `Serve` returns an error when requests had to be cancelled, so the process exits with status 1. Failing readiness first arrives with `/readyz` in M6.

**Rejected.** Relying on `Close` to cancel handlers: they would see an ordinary disconnect and couldn't tell the client why. A config field for the 5s unwind time: nothing needs to tune it yet.

## 24. A write deadline for each stream event (M2)

**Context.** One read-and-write loop gives backpressure for free: a slow client makes the write block, so the loop reads the upstream more slowly too. But a client that stops reading altogether would block a write forever, and hold the upstream stream open with it.

**Decision.** Before each event the stream loop sets a write deadline of `timeouts.idle` through `http.ResponseController`, and clears it when the stream ends. It has to be cleared because the server has no `WriteTimeout`, so net/http won't reset the deadline for the next request on a kept-alive connection.

**Rejected.** A server-wide `WriteTimeout`: it limits the whole response, and would cut off long streams.

## 25. Smaller M2 choices (M2)

- **The SSE writer lives in `internal/api`,** not `internal/server` as the brief's layout says, because the mock's HTTP server writes the same events. Otherwise the mock would have to import the gateway's server package.
- **Timing tests run in `testing/synctest` over an in-memory `net.Pipe` network,** so the real `http.Server` and `http.Client` run on the fake clock. That makes "each event arrives when it's written" checkable to the millisecond, with no real sleeps. The disconnect test uses real TCP and the real clock instead, because it measures how fast net/http notices a closed connection. The SIGTERM drain test runs the real binary.
- **An exercise stub returns a package-level error variable,** not an inline `errors.New`, so staticcheck can't conclude that callers' error checks are dead code.
- **The mock gained `fail_at_chunk` and `stall_at_chunk`** for error events and stalls at a chosen point in a stream. `hang` still blocks before any response, the way unreturned response headers would.

## 26. Adapters on raw net/http, one client per provider (M3)

**Context.** The brief rules out provider SDKs, `http.DefaultClient` and a blanket client timeout.

**Decision.** Each real provider gets its own `http.Client` from `provider.NewHTTPClient`, so each has its own connection pool. The transport settings are explicit: `timeouts.connect` for dialing and the TLS handshake, TCP keep-alive, and up to 512 idle connections per host, because the default of 2 would make a busy gateway open and close connections constantly. There is no `Client.Timeout`: every call is bounded by its context, and streams by the router's watchdog. Error bodies are read up to 64 KiB and responses up to 16 MiB, so a broken upstream can't exhaust memory. Upstream SSE is parsed by a small `internal/sse` package, which isn't in the brief's layout but is shared by both adapters.

**Rejected.** One shared client for all providers: a slow provider's connections would compete with everyone else's for the same pool limits.

## 27. How upstream failures are classified (M3)

**Decision.** On top of the brief's table:

- **A 404 or 402 is a config error,** not a bad request. A 404 for a configured model means the gateway's own model ID is wrong, and a 402 is the gateway's own billing; neither is the client's fault, so the router falls back and logs loudly.
- **An OpenAI 429 about money is a config error, not a rate limit.** That covers `insufficient_quota`, credit exhausted, and the spend and usage limits. Waiting won't fix it, so the router falls back instead of retrying.
- **Errors inside streams.** An OpenAI error object mid-stream is Unavailable. An Anthropic `error` event is classified by its type: `rate_limit_error` is RateLimited, and everything else, `overloaded_error` included, is Unavailable.
- **A stream that ends early is a failure.** That means one ending without OpenAI's `[DONE]` or Anthropic's `message_stop`. The error must not wrap `io.EOF`, because callers read `io.EOF` as a complete stream. A test caught exactly that bug.
- **A cancelled context wins.** A transport error during a call whose context has ended is reported as that context's error, so a client disconnect is never mistaken for an upstream failure.

**Rejected.** Classifying by status code alone: a quota 429 would be retried for nothing, and a typo in a model ID would reach the client as its own bad request.

## 28. Request rules that differ by provider and model (M3)

**Context.** Brief sections 6.2 and 7: sampling parameters differ by provider and model. Current Claude models (Opus 4.7 and later, Sonnet 5 and later, Haiku 5.5) reject any non-default `temperature`, `top_p` or `top_k`. Haiku 5.5 also rejects `temperature` and `top_p` together. Older Claude models take `temperature` from 0 to 1. OpenAI reasoning models restrict sampling too.

**Decision.** Two layers, both checked against every target of an alias before the first attempt:

1. **Per target:** `sampling: false` in the config marks a model that takes no sampling parameters. A request that sets `temperature` or `top_p` for that alias gets a 400 naming the parameter.
2. **Per provider:** an adapter can implement `Validate`. Anthropic's checks that temperature is from 0 to 1, that `temperature` and `top_p` aren't both set, and that every user and assistant message has some text.

Invalid requests are rejected, never clamped or dropped.

**Rejected.** Clamping or dropping values: the client would get different output from what it asked for, without being told. Guessing support from the model name: model names change faster than code.

## 29. Translating to Anthropic's Messages API (M3)

**Decision.**

- **System prompt:** `system` and `developer` messages, wherever they appear, become the top-level `system` blocks in order. Their position in the conversation is lost; Anthropic has no system role inside it.
- **Content:** each text part becomes its own block, and empty parts are dropped, because Anthropic rejects empty text blocks.
- **Usage:** `prompt_tokens` is input plus cache-creation plus cache-read tokens, since prompt-cache discounts aren't modelled.
- **Stop reasons:** `max_tokens` and `model_context_window_exceeded` map to `length`, and `refusal` to `content_filter`. Everything else, unknown reasons included, maps to `stop`.
- **Thinking:** left out of non-streaming replies; it becomes reasoning chunks in streams.
- **Unknown stream events and delta types** are keepalives, as Anthropic's versioning policy asks.
- **Prefill:** a final assistant message, which recent models reject, isn't checked; the rule depends on the model, and the upstream 400 comes back to the client as a bad request.

## 30. Hidden reasoning counts as the first token but doesn't commit (M3)

**Context.** Recent Claude models think by default, streaming thinking for a while before any text. Under decision 21 the first-token timeout would fire during a long think, and the gateway would retry a request that was working.

**Decision.** A chunk can be marked `Reasoning`. The first output of any kind, text or reasoning, ends the first-token timeout and starts the idle timeout. The commit point is still the first text the client will see, so a failure during thinking can still fall back. A model that stalls after thinking hits the idle timeout. OpenAI's reasoning tokens aren't streamed in Chat Completions, so a reasoning model there needs a `first_token` timeout long enough for its thinking.

**Rejected.** Treating thinking as keepalives: they don't show progress, so a long think would time out. Committing on thinking: the client would get a 200 with nothing in it, and a later failure couldn't fall back.

## 31. Retries, fallback and breakers (M3)

**Decision.** For each target in order:

- **Breaker open:** the target is skipped.
- **Unavailable:** retried up to `max_attempts_per_target` times, after an exponential backoff with full jitter (the M3 exercise).
- **Rate limited:** waits out its `Retry-After` and retries, if the hint is no longer than `max_retry_after`. Otherwise, or with no hint, it falls back at once.
- **Auth or config:** falls back without a retry.
- **Bad request or cancellation:** stops everything.

Each breaker hands out permits. Exactly one caller gets the half-open probe, results from calls let through before the breaker opened are ignored, and a neutral outcome (a rate limit, a bad request, a cancellation) neither counts nor closes it. A stream that fails after the commit point counts against its target's breaker too.

When every target fails, the router returns an `ExhaustedError`:

- if each target was only skipped or rate limited, it is a 503 (`upstream_unavailable` or `upstream_rate_limited`) with `Retry-After` set to the shortest hint or breaker cool-down;
- if time ran out, a 504;
- otherwise a 502, including a mix of rate limits and outright failures.

Non-streaming calls have only the total deadline, because there is no first token to watch.

**Rejected.** Retrying on auth errors: a wrong key stays wrong. Waiting out any `Retry-After`: one rate-limited provider could hold a request for a minute while another provider was free.

## 32. The per-attempt story goes in the Result (M3)

**Decision.** The router returns every attempt and skip in `Result.Tries` (target, outcome, error, duration). The server logs them with the request ID, which the router doesn't know about. They are logged when a request failed, or when it succeeded but needed more than one try, and at error level whenever any target had an auth or config problem, so a broken key is loud even when a fallback hides it from clients. `X-Tollgate-Attempts` counts calls made, not skips.

**Rejected.** Logging from inside the router: it would need the server's request ID, and `internal/server` already imports `internal/router`.

## 33. A provider without its key is disabled, not fatal (M3)

**Context.** Brief section 19: `docker compose up` must work, and the mock alias must answer, without any provider keys.

**Decision.** If a real provider's `api_key_env` variable is empty, startup logs a warning and the provider is replaced by one that fails every call with a config error. Aliases that use it fall back past it, logged at error level; aliases that don't are unaffected. `make run` and `make smoke` load `.env`, and `.env.example` lists the variables. The real-API tests have the `smoke` build tag, so `make test` and CI never call a paid API; they skip when a key or model isn't set.

**Rejected.** Failing startup: the gateway couldn't run without every key, even for the mock. Skipping the provider silently: a missing key would only show up as unexplained fallbacks.

## 34. Long-context price tiers (M3)

**Context.** Claude Haiku 5.5, behind the `fast` alias, has two prices. A request whose prompt is over 100,000 tokens pays $0.50 instead of $0.10 per million input tokens, and $2.50 instead of $0.50 per million output tokens, for the whole request. The prompt count includes cached tokens. With one price per target, those requests would be recorded at a fifth of their cost.

**Decision.** A price can have one optional tier: `long_context: { above_prompt_tokens, input, output }`. `Price.Rates(promptTokens)` returns the tier's prices when the prompt is strictly over the threshold, and the base prices otherwise. Startup rejects a tier with a threshold below 1, or with either price lower than the base price.

The M5 budget hold prices its upper bound on the prompt with the same rule, target by target, and takes the largest. That is only an upper bound if crossing the threshold never lowers a price, which is why cheaper tier prices are rejected. Settling uses the prompt tokens the provider reports. If usage is estimated, the estimate picks the tier.

**Rejected.** Always charging the higher price: most requests would be recorded at five times their cost. A list of tiers: no current price list has more than one threshold, and one tier is easier to read and validate. Capping prompts below the threshold: a client sending a long prompt should pay the long-context price, not get an error.

## 35. Postgres access: pgx, sqlc and goose (M4)

**Decision.** One `pgxpool` pool. sqlc generates typed queries into `internal/store/db`, reading the schema from the goose migrations themselves; the generated code is committed, and `make generate` rebuilds it. The migrations are embedded in the binary and applied only by `tollgate migrate`, under a Postgres advisory lock so that concurrent runs take turns. `serve` refuses to start while any migration is pending. Pool settings go in the `DATABASE_URL` query string (`pool_max_conns=10`), so the config file has no database section.

**Rejected.** Migrating when `serve` starts: a schema change should be a deliberate step, and a failing migration would become a crash loop. A separate `schema.sql` for sqlc: two copies of the schema that can drift apart.

## 36. Virtual keys (M4)

**Decision.** A key is `tg_` plus 32 random bytes in lowercase base32, 55 characters in all. The database stores the SHA-256 hash (`bytea`, unique) and a display prefix of 11 characters (`tg_` and 40 bits of the secret). A key that isn't well formed is rejected before any lookup. `allowed_models` is NULL for every alias; an empty array is rejected by a CHECK, so "no restriction" has one spelling.

A fast hash is enough because the secret is 256 random bits. Slow hashes like bcrypt protect low-entropy passwords from guessing; nothing can guess a 256-bit secret, and every request has to hash the key it presents. A slow hash would add tens of milliseconds to each cache miss for no gain.

**Rejected.** base64url: its `-` and `_` break double-click selection. Hex: 64 characters for the same randomness.

## 37. Key cache and revocation delay (M4)

**Decision.** Keys found in the database are cached for `auth.cache_ttl`, default 30s and at most 5m. Revoked keys are cached as revoked. The revocation delay is therefore the TTL: a revoked key works for at most 30s more. Unknown keys and lookup failures aren't cached. A failed lookup is a 503 `auth_unavailable` with `Retry-After: 1`, not a 401, so a database outage doesn't look like a bad key, which clients don't retry.

**Rejected.** Caching unknown keys: anyone could grow the cache by sending made-up keys. Each one costs one indexed lookup instead, bounded by the in-flight cap. Serving stale entries while the database is down: revocation would stop working for as long as the outage lasted. singleflight for concurrent misses: two lookups of the same key at once are rare and harmless.

## 38. Rate limits: token buckets, reservations and refunds (M4)

**Decision.** Each limit is a token bucket whose capacity is the per-minute limit. It refills continuously at limit/60 per second and starts full, so a key can spend a minute's allowance at once and then continue at the steady rate. Tokens are float64: refilling earns fractions, and nothing here is money.

The registry holds one pair of buckets per key, under one mutex. During any call, at most once a minute, it drops the pairs whose buckets are both full. A new pair would be identical, so dropping one changes no decision. This means there is no background goroutine to own. Limits are passed in on each call, and a pair whose key's limits changed is resized. The server sees the registry through an interface, so a limiter shared between instances could replace it. Until then, limits apply per instance.

Requests per minute are counted before the body is read, as in the pipeline, so a malformed request counts too. Tokens per minute are reserved after validation: the estimated prompt (about four characters per token) plus the request's maximum output. A deferred refund settles the reservation after the response is written, so every exit path settles it, panics included. The refund returns reserved minus used. "Used" is the provider's count, or an estimate if it reported none; a request that failed before producing anything used 0. If a request used more than it reserved, the difference is taken, and the bucket can go below zero.

A request that needs more tokens than the whole TPM limit gets OpenAI's response for this case: 429, type `tokens`, code `rate_limit_exceeded`, "Request too large", and no `Retry-After`, since waiting won't help.

**Rejected.**
- An integer GCRA: exact, but harder to explain, and the brief asks for a token bucket.
- A sweeper goroutine: one more goroutine to start, stop and test, for something a call can do in passing.
- Refunds that never charge overuse: a key whose prompts are consistently underestimated could exceed its limit.
- A 400 for a request too large for the limit: OpenAI clients expect a 429 there.

## 39. Authentication responses (M4)

**Decision.** Every path under `/v1/` needs a key, including unknown paths; `/healthz` doesn't. Only `Authorization: Bearer`, which is what OpenAI SDKs send; the scheme is case-insensitive. A missing, unknown or revoked key gets a 401 `invalid_api_key` with `WWW-Authenticate: Bearer`, and a message saying which. An alias the key may not use gets the same 404 as an alias that doesn't exist, in OpenAI's words: "does not exist or you do not have access to it". `/v1/models` lists only the aliases the key may use.

**Rejected.** A 403 for a disallowed alias: it would tell a key which aliases exist.

## 40. Postgres in tests (M4)

**Decision.** Tests that need Postgres get a schema of their own in one database: `storetest` creates it, migrates it, points `search_path` at it through the URL, and drops it afterwards, so tests can run in parallel. They are skipped when `TOLLGATE_TEST_DATABASE_URL` is empty, except in CI (where GitHub sets `CI`): there a missing URL fails the test, so the database tests can't be skipped by accident. CI runs Postgres 18 as a service container. Locally, `docker-compose.yml` has only Postgres for now (`make db`); the rest of the compose file arrives in M7.

**Rejected.** A build tag: the URL has to come from somewhere anyway, and a tag is easy to forget. testcontainers: another dependency, and it still needs Docker.
