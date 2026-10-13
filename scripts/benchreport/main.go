// Command benchreport turns a directory of loadgen JSON reports, written by
// scripts/bench.sh, into the markdown tables in docs/BENCHMARKS.md.
//
//	go run ./scripts/benchreport bench-results/20261013T120000Z
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// report is the part of a loadgen JSON report used here.
type report struct {
	Requests  int            `json:"requests"`
	PerSecond float64        `json:"requests_per_second"`
	Errors    map[string]int `json:"errors"`
	Latency   percentiles    `json:"latency_ms"`
	TTFT      *percentiles   `json:"ttft_ms"`
}

type percentiles struct {
	P50, P95, P99, Max float64
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchreport DIR")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "benchreport:", err)
		os.Exit(1)
	}
}

func run(dir string, w io.Writer) error {
	reports := map[string]report{}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var r report
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		reports[strings.TrimSuffix(filepath.Base(f), ".json")] = r
	}
	if setup, err := os.ReadFile(filepath.Join(dir, "setup.txt")); err == nil {
		fmt.Fprintf(w, "```\n%s```\n\n", setup)
	}

	levels := concurrencyLevels(reports)
	for _, mode := range []string{"nonstream", "stream"} {
		title := "Non-streaming"
		if mode == "stream" {
			title = "Streaming"
		}
		fmt.Fprintf(w, "### %s: throughput and latency\n\n", title)
		fmt.Fprintln(w, "| Workers | Path | Requests/s | p50 | p95 | p99 | Errors |")
		fmt.Fprintln(w, "|---:|---|---:|---:|---:|---:|---|")
		for _, c := range levels {
			for _, path := range []string{"direct", "gateway", "budget"} {
				r, ok := reports[fmt.Sprintf("%s-%s-c%d", mode, path, c)]
				if !ok {
					continue
				}
				fmt.Fprintf(w, "| %d | %s | %.0f | %.1f | %.1f | %.1f | %s |\n",
					c, pathName(path), r.PerSecond, r.Latency.P50, r.Latency.P95, r.Latency.P99, errorList(r))
			}
		}
		fmt.Fprintln(w)

		fmt.Fprintf(w, "### %s: gateway overhead in ms (through the gateway minus direct)\n\n", title)
		if mode == "stream" {
			fmt.Fprintln(w, "| Workers | Key | p50 | p95 | p99 | TTFT p50 | TTFT p95 | TTFT p99 |")
			fmt.Fprintln(w, "|---:|---|---:|---:|---:|---:|---:|---:|")
		} else {
			fmt.Fprintln(w, "| Workers | Key | p50 | p95 | p99 |")
			fmt.Fprintln(w, "|---:|---|---:|---:|---:|")
		}
		for _, c := range levels {
			direct, ok := reports[fmt.Sprintf("%s-direct-c%d", mode, c)]
			if !ok {
				continue
			}
			for _, path := range []string{"gateway", "budget"} {
				r, ok := reports[fmt.Sprintf("%s-%s-c%d", mode, path, c)]
				if !ok {
					continue
				}
				key := "no budget"
				if path == "budget" {
					key = "budget"
				}
				d := diff(r.Latency, direct.Latency)
				fmt.Fprintf(w, "| %d | %s | %+.1f | %+.1f | %+.1f |", c, key, d.P50, d.P95, d.P99)
				if mode == "stream" && r.TTFT != nil && direct.TTFT != nil {
					t := diff(*r.TTFT, *direct.TTFT)
					fmt.Fprintf(w, " %+.1f | %+.1f | %+.1f |", t.P50, t.P95, t.P99)
				}
				fmt.Fprintln(w)
			}
		}
		fmt.Fprintln(w)
	}

	if d, ok := reports["ceiling-direct"]; ok {
		g := reports["ceiling-gateway"]
		fmt.Fprintln(w, "### Throughput ceiling: zero-latency mock, 200 workers, non-streaming")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| Path | Requests/s | p50 | p95 | p99 | Errors |")
		fmt.Fprintln(w, "|---|---:|---:|---:|---:|---|")
		for _, row := range []struct {
			name string
			r    report
		}{{"direct to the mock", d}, {"through the gateway", g}} {
			fmt.Fprintf(w, "| %s | %.0f | %.2f | %.2f | %.2f | %s |\n",
				row.name, row.r.PerSecond, row.r.Latency.P50, row.r.Latency.P95, row.r.Latency.P99, errorList(row.r))
		}
		fmt.Fprintln(w)
	}
	if rec, err := os.ReadFile(filepath.Join(dir, "reconcile.txt")); err == nil {
		fmt.Fprintf(w, "Reconcile after the budgeted runs: `%s`\n", strings.TrimSpace(string(rec)))
	}
	return nil
}

// concurrencyLevels returns the worker counts that appear in the report
// names, in order.
func concurrencyLevels(reports map[string]report) []int {
	var levels []int
	for name := range reports {
		_, after, ok := strings.Cut(name, "-c")
		if n, err := strconv.Atoi(after); ok && err == nil && !slices.Contains(levels, n) {
			levels = append(levels, n)
		}
	}
	slices.Sort(levels)
	return levels
}

func pathName(p string) string {
	switch p {
	case "direct":
		return "direct to the mock"
	case "gateway":
		return "gateway, no budget"
	default:
		return "gateway, budget"
	}
}

func errorList(r report) string {
	if len(r.Errors) == 0 {
		return "none"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(r.Errors)) {
		parts = append(parts, fmt.Sprintf("%s: %d", k, r.Errors[k]))
	}
	return strings.Join(parts, ", ")
}

func diff(a, b percentiles) percentiles {
	return percentiles{P50: a.P50 - b.P50, P95: a.P95 - b.P95, P99: a.P99 - b.P99, Max: a.Max - b.Max}
}
