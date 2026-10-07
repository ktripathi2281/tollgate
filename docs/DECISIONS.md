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
