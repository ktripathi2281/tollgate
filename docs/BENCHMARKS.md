# Benchmarks

What the gateway costs in latency and throughput, measured against a mock upstream (brief section 17).

**The upstream is a mock on loopback.** These numbers are the gateway's own cost: parsing, auth, rate limits, routing, the upstream HTTP call, streaming, logging and, for a budgeted key, two Postgres transactions. They say nothing about any real provider. Everything ran on one laptop, so the load generator, the gateway, the mock and Postgres competed for the same CPUs. No comparison with other gateways is made, because none was run under the same conditions.

## Method

`make bench` runs `scripts/bench.sh`, which:

1. Builds the binaries, applies the migrations, and creates two keys with limits far above the load: one without a budget, and one with a budget large enough never to refuse a request.
2. Starts `mockupstream` as a separate process with a fixed latency: the first token after 50 ms, then one every 5 ms, 20 tokens in all.
3. Starts the gateway with `deploy/bench.yaml`. Its one alias, `bench`, reaches the mock over HTTP as an OpenAI-compatible provider, so a request takes the same path as one to a real provider. Logs go to a file on the Linux filesystem, at the default `info` level, so each request still writes its access-log line.
4. For non-streaming, then streaming requests, and for 50, 200 and 1,000 concurrent workers, runs `cmd/loadgen` for 20 seconds each:
   - straight at the mock: the baseline;
   - through the gateway with the key without a budget;
   - through the gateway with the budgeted key.

   Each worker sends its next request as soon as the last one finishes, with no target rate. Overhead is the gateway's latency minus the baseline's at p50, p95 and p99, and the same for time to first token on streams.
5. Stops the gateway, which drains its request log, and runs `tollgate reconcile`, which must pass.
6. Restarts the mock with no latency and runs 200 workers straight at the mock, then through the gateway, to find the throughput ceiling. A 10-second CPU profile of the gateway is taken from its admin listener during that run.
7. Prints the tables below (`scripts/benchreport`). The raw JSON, the profile and its top functions are kept in `bench-results/`, which git ignores.

```bash
make db                   # Postgres in Docker
cp .env.example .env      # if there isn't one: DATABASE_URL for the local Postgres
make bench                # about 8 minutes; BENCH_DURATION and BENCH_CONCURRENCY override the defaults
```

## Setup

```
machine: 12th Gen Intel(R) Core(TM) i5-12450H, 12 hardware threads, 7 GB, Windows 11 with WSL2 (Ubuntu)
go: go1.27.1, GOMAXPROCS 12 (the default: one per hardware thread)
postgres: postgres (PostgreSQL) 18.6, in Docker Desktop on the same machine
duration per run: 20s; concurrency: 50 200 1000
date: 2026-10-13, commit with the single-statement holds (see "The fix" below)
```

## Results

### Non-streaming: throughput and latency (ms)

| Workers | Path | Requests/s | p50 | p95 | p99 | Errors |
|---:|---|---:|---:|---:|---:|---|
| 50 | direct to the mock | 330 | 151.3 | 153.6 | 156.0 | none |
| 50 | gateway, no budget | 326 | 152.8 | 156.0 | 160.3 | none |
| 50 | gateway, budget | 303 | 159.7 | 189.7 | 232.0 | none |
| 200 | direct to the mock | 1311 | 151.9 | 155.6 | 160.1 | none |
| 200 | gateway, no budget | 1291 | 153.3 | 157.8 | 163.9 | none |
| 200 | gateway, budget | 299 | 661.7 | 722.0 | 755.8 | none |
| 1000 | direct to the mock | 6521 | 151.6 | 155.9 | 164.5 | none |
| 1000 | gateway, no budget | 6422 | 152.8 | 161.8 | 182.5 | none |
| 1000 | gateway, budget | 298 | 3335.2 | 3467.7 | 3511.6 | none |

### Non-streaming: gateway overhead in ms (through the gateway minus direct)

| Workers | Key | p50 | p95 | p99 |
|---:|---|---:|---:|---:|
| 50 | no budget | +1.4 | +2.3 | +4.3 |
| 50 | budget | +8.3 | +36.0 | +76.0 |
| 200 | no budget | +1.4 | +2.2 | +3.8 |
| 200 | budget | +509.8 | +566.5 | +595.6 |
| 1000 | no budget | +1.1 | +6.0 | +18.0 |
| 1000 | budget | +3183.5 | +3311.8 | +3347.1 |

### Streaming: throughput and latency (ms)

| Workers | Path | Requests/s | p50 | p95 | p99 | Errors |
|---:|---|---:|---:|---:|---:|---|
| 50 | direct to the mock | 309 | 161.8 | 166.6 | 169.9 | none |
| 50 | gateway, no budget | 310 | 159.8 | 165.1 | 169.1 | none |
| 50 | gateway, budget | 280 | 172.6 | 207.3 | 251.1 | none |
| 200 | direct to the mock | 1249 | 159.3 | 165.0 | 169.7 | none |
| 200 | gateway, no budget | 1242 | 159.6 | 165.4 | 171.9 | none |
| 200 | gateway, budget | 269 | 733.5 | 805.0 | 845.9 | none |
| 1000 | direct to the mock | 6213 | 159.1 | 167.5 | 176.9 | none |
| 1000 | gateway, no budget | 5342 | 182.5 | 215.6 | 248.3 | none |
| 1000 | gateway, budget | 270 | 3695.1 | 3906.3 | 4216.6 | none |

### Streaming: gateway overhead in ms (through the gateway minus direct)

| Workers | Key | p50 | p95 | p99 | TTFT p50 | TTFT p95 | TTFT p99 |
|---:|---|---:|---:|---:|---:|---:|---:|
| 50 | no budget | -1.9 | -1.5 | -0.7 | +0.7 | +1.4 | +3.9 |
| 50 | budget | +10.9 | +40.7 | +81.3 | +7.7 | +32.4 | +61.6 |
| 200 | no budget | +0.2 | +0.3 | +2.2 | +0.9 | +2.6 | +6.6 |
| 200 | budget | +574.2 | +639.9 | +676.2 | +370.7 | +435.5 | +468.1 |
| 1000 | no budget | +23.4 | +48.1 | +71.4 | +7.7 | +30.5 | +52.0 |
| 1000 | budget | +3535.9 | +3738.8 | +4039.7 | +1887.0 | +2198.8 | +3083.4 |

A negative overhead is noise: the two runs are 20 seconds apart, and at 50 workers their difference is within run-to-run variation.

### Throughput ceiling: zero-latency mock, 200 workers, non-streaming

| Path | Requests/s | p50 | p95 | p99 | Errors |
|---|---:|---:|---:|---:|---|
| direct to the mock | 76434 | 1.12 | 9.71 | 14.44 | none |
| through the gateway | 24919 | 6.81 | 18.33 | 24.28 | none |

Reconcile after the budgeted runs: `checked 3 budget periods: 0 mismatched`.

## What the numbers say

**Without a budget, the gateway costs about 1.5 ms at the median.** That holds up to 1,000 workers non-streaming, and for streams up to 200 workers, with time to first token within a millisecond of the mock's. That time covers auth from the key cache, both rate limits, validation, the extra HTTP hop and SSE translation, the access log and the request-log row.

**Streaming at 1,000 workers runs out of CPU.** That's the machine, not the gateway's design. During that run the machine was 86% busy across its 12 threads:

| Process | Cores |
|---|---:|
| the gateway | 5.8 |
| the mock | 2.8 |
| the load generator | 1.7 |

The same run without streaming kept it 45% busy. A stream costs the gateway about 1.1 ms of CPU, against 0.5 ms for a non-streaming request, because it parses and writes 21 events. On a machine to itself, the gateway would have that CPU.

**The ceiling, with a mock that answers at once, is about 25,000 requests a second through the gateway,** using about 5.2 cores, against 76,000 straight to the mock. The mock does almost nothing per request, so this measures what the gateway's work per request costs.

**A key with a budget is capped at about 300 requests a second.** That's for one key; different keys don't share the cap. Below that rate, the budget adds about 8 ms at the median, for two transactions per request. Above it, requests queue, and latency grows with the queue: 3.3 s at the median with 1,000 workers. The next two sections explain the cap and what was done about it.

## The fix: budget transactions as single statements

The first full run capped a budgeted key at **215 requests a second**, whatever the concurrency. All of a key's holds and settles update the same `budget_periods` row, so they take turns on its row lock. Each was a multi-statement transaction:

- `BEGIN`, a conditional `UPDATE` (which takes the lock), an `INSERT`, `COMMIT`.

So the row stayed locked across several network round trips, and requests queued for it. The CPU profile showed nothing, because the gateway was waiting, not working.

The fix makes each operation a single statement, using a data-modifying CTE. A hold becomes "UPDATE the period if the budget allows, then INSERT the hold from the updated row", in one statement. A settle becomes "UPDATE the hold if it is held, then UPDATE the period from it". The lock is held only while Postgres runs the statement, never across the network. The guarantees are unchanged:
- the conditional UPDATE still re-checks the budget after waiting for the lock;
- the status guard still makes settles and releases idempotent;
- every money test still passes, including the 200-request budget test and every exit path.

| Budgeted key, non-streaming | Before | After |
|---|---:|---:|
| 50 workers: requests/s | 216 | 303 |
| 50 workers: overhead p50 / p95 / p99 (ms) | +72.6 / +142.4 / +187.1 | +8.3 / +36.0 / +76.0 |
| 200 workers: requests/s | 211 | 299 |
| 1000 workers: requests/s | 214 | 298 |
| 1000 workers: latency p50 (ms) | 4599.9 | 3335.2 |

## What still limits a budgeted key: the disk flush

After the fix, a budgeted key is capped at **about 300 requests a second**. Postgres releases a row lock at commit, and with `synchronous_commit = on` (the default), a commit waits for the write-ahead log to be flushed to disk. A budgeted request commits on its key's row twice, once for the hold and once for the settle, so each request holds the row through two flushes.

On this machine, Postgres runs in Docker Desktop's VM, where a flush (`pg_test_fsync`, `fdatasync`) takes **1.28 ms**. Two flushes, plus running the statements, come to about 3.3 ms per request. One request at a time is 300 a second.

Turning `synchronous_commit` off for one diagnostic run confirmed it: the same budgeted load at 200 workers went from 294 to 1,255 requests a second, the same as without a budget, at 154 ms p50. That setting isn't used: a database crash could lose settles that the gateway had already treated as committed, and with them recorded spend.

So **one key's budgeted throughput is bounded by about 1 / (2 × the database's flush time).** On a disk that flushes in 0.1 ms, that is a few thousand requests a second. Different keys don't contend, because each has its own row. Spreading one key's counters over several rows would raise the cap, at the cost of a budget check over several rows; it isn't done.

## CPU profile at the ceiling

`go tool pprof` on 10 seconds of the gateway at about 25,000 requests a second: 52 s of CPU samples, 5.2 cores. The biggest costs, cumulative:

| Where | Share of samples |
|---|---:|
| `handleChatCompletions`, all of it | 35% |
| the upstream call (`openai.Provider.Chat`, over HTTP) | 18% |
| socket system calls (`Syscall6`): reading and writing the client and upstream connections, and the log | 15% |
| allocation (`mallocgc`), plus about 4% more in the garbage collector's marking | 10% |
| JSON decoding (`encoding/json`): the request, and the upstream's response | 9% |
| reading upstream responses (`persistConn.readLoop`) | 9% |
| parsing and validating the request (`api.ParseChatCompletionRequest`) | 8% |
| the access log (`slog`) | 5% |

No single function in the gateway's own code dominates. The costs are the work a proxy has to do: two HTTP exchanges per request, two JSON documents to decode, and a log line. The candidates were:
- buffering the access log, which would save some system calls but risk losing log lines in a crash;
- a hand-written JSON decoder, which would trade clarity for a few percent.

Neither was worth it next to the budget-row fix above, which was the change worth making.

## Caveats

- **One machine.** The load generator, the gateway, the mock and Postgres shared one laptop's 12 hardware threads, so heavy runs measure the machine as much as the gateway. Postgres ran in Docker Desktop's VM on a virtual disk with slow flushes.
- **A closed loop.** Each worker waits for its response before sending the next, so the offered load falls when latency rises, and queueing beyond the workers isn't measured (coordinated omission). The numbers say what the gateway sustains, not how it behaves under a fixed arrival rate.
- **The upstream is a mock on loopback.** Real providers add tens to thousands of milliseconds, beside which the gateway's 1.5 ms is small. The budget cap, though, is per key whatever the provider.
- **Every run used one key.** A deployment with many keys spreads budget writes over many rows.
