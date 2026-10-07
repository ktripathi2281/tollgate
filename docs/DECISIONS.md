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
