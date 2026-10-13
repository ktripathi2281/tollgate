#!/usr/bin/env bash
# Runs the benchmarks in docs/BENCHMARKS.md (brief section 17) and prints
# the same tables. Needs Postgres (make db) and DATABASE_URL, which
# `make bench` loads from .env.
#
#   make bench                        # 20s per run
#   BENCH_DURATION=10s make bench
#   BENCH_CONCURRENCY="50 200" make bench
#
# Everything runs on this machine: the load generator, the mock upstream,
# the gateway and, through Docker, Postgres. The upstream is a mock on
# loopback, so these numbers are the gateway's own cost, not a provider's.
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=${BENCH_DURATION:-20s}
LEVELS=${BENCH_CONCURRENCY:-50 200 1000}
OUT=bench-results/$(date -u +%Y%m%dT%H%M%SZ)
: "${DATABASE_URL:?set DATABASE_URL, for example with cp .env.example .env}"

MOCK=127.0.0.1:9191
GATEWAY=127.0.0.1:8181
ADMIN=127.0.0.1:6161
LOGS=$(mktemp -d)
mkdir -p "$OUT"

pids=()
cleanup() {
	for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$LOGS"
}
trap cleanup EXIT

wait_for() {
	for _ in $(seq 100); do
		curl -sf -o /dev/null "$1" && return 0
		sleep 0.1
	done
	echo "$1 didn't come up" >&2
	exit 1
}

start_mock() {
	./bin/mockupstream -addr "$MOCK" "$@" >"$LOGS/mock.log" 2>&1 &
	MOCK_PID=$!
	pids+=("$MOCK_PID")
	wait_for "http://$MOCK/v1/models"
}

start_gateway() {
	# Logs go to the Linux filesystem: writing them to a Windows mount
	# under WSL would slow the gateway down.
	BENCH_UPSTREAM_KEY=unused ./bin/tollgate serve --config deploy/bench.yaml >"$LOGS/gateway.log" 2>&1 &
	GATEWAY_PID=$!
	pids+=("$GATEWAY_PID")
	wait_for "http://$GATEWAY/readyz"
}

stop() {
	kill "$1"
	wait "$1" 2>/dev/null || true
}

# loadgen NAME URL [flags]: one run, saved as JSON.
loadgen() {
	local name=$1 url=$2
	shift 2
	./bin/loadgen -url "$url" -d "$DURATION" -json "$@" >"$OUT/$name.json"
	echo "  $name done"
}

make build >/dev/null
./bin/tollgate migrate >/dev/null
key() { ./bin/tollgate keys create -name "$1" -rpm 100000000 -tpm 2000000000 "${@:2}" | grep -o 'tg_[a-z2-7]*'; }
PLAIN=$(key bench-plain)
BUDGETED=$(key bench-budgeted -budget 1000000 -period monthly)

{
	echo "machine: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs), $(nproc) hardware threads, $(free -g | awk '/Mem:/ {print $2}') GB"
	echo "go: $(go env GOVERSION), GOMAXPROCS ${GOMAXPROCS:-$(nproc)} (the default: one per hardware thread)"
	echo "postgres: $(docker compose exec -T postgres postgres --version 2>/dev/null || echo unknown), in Docker on the same machine"
	echo "duration per run: $DURATION; concurrency: $LEVELS"
} | tee "$OUT/setup.txt"

# 1. Overhead: a mock whose first token comes after 50ms, then one every
# 5ms, 20 in all. Each level runs against the mock directly, then through
# the gateway with a key without a budget, then with one.
echo "== fixed-latency runs"
start_mock -ttft 50ms -token-interval 5ms -output-tokens 20
start_gateway
for mode in nonstream stream; do
	flag=()
	[ "$mode" = stream ] && flag=(-stream)
	for c in $LEVELS; do
		loadgen "$mode-direct-c$c" "http://$MOCK/v1" -model mock-1 -c "$c" "${flag[@]}"
		loadgen "$mode-gateway-c$c" "http://$GATEWAY/v1" -key "$PLAIN" -model bench -c "$c" "${flag[@]}"
		loadgen "$mode-budget-c$c" "http://$GATEWAY/v1" -key "$BUDGETED" -model bench -c "$c" "${flag[@]}"
	done
done
stop "$GATEWAY_PID" # drains the request log before the reconcile
echo "== reconcile"
./bin/tollgate reconcile | tee "$OUT/reconcile.txt"

# 2. The throughput ceiling: a mock that answers at once, and a CPU
# profile of the gateway taken during the run.
echo "== zero-latency runs"
stop "$MOCK_PID"
start_mock -ttft 0 -token-interval 0 -output-tokens 20
start_gateway
loadgen ceiling-direct "http://$MOCK/v1" -model mock-1 -c 200
loadgen ceiling-gateway "http://$GATEWAY/v1" -key "$PLAIN" -model bench -c 200 &
LOAD_PID=$!
sleep 3
curl -s "http://$ADMIN/debug/pprof/profile?seconds=10" >"$OUT/ceiling.prof"
wait "$LOAD_PID"
go tool pprof -top -nodecount=30 bin/tollgate "$OUT/ceiling.prof" >"$OUT/ceiling-top.txt" 2>/dev/null
go tool pprof -top -cum -nodecount=60 bin/tollgate "$OUT/ceiling.prof" >"$OUT/ceiling-top-cum.txt" 2>/dev/null
stop "$GATEWAY_PID"

echo
go run ./scripts/benchreport "$OUT"
echo "Raw results, the CPU profile and its top functions: $OUT"
