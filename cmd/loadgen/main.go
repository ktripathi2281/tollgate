// Command loadgen sends chat-completion requests to a gateway, or straight
// to an upstream, and reports throughput, errors and latency percentiles
// (brief section 17).
//
//	loadgen -url http://localhost:8080/v1 -key tg_... -model mock-fast -c 50 -d 30s -stream
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

// options are the command's flags.
type options struct {
	url         string
	key         string
	model       string
	concurrency int
	duration    time.Duration
	rate        float64
	stream      bool
	maxTokens   int
	prompt      string
	json        bool
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var o options
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.url, "url", "http://localhost:8080/v1", "base URL of an OpenAI-compatible API")
	fs.StringVar(&o.key, "key", os.Getenv("TOLLGATE_KEY"), "API key to send as a bearer token (default $TOLLGATE_KEY; empty sends none)")
	fs.StringVar(&o.model, "model", "mock-fast", "model to request")
	fs.IntVar(&o.concurrency, "c", 50, "concurrent workers")
	fs.DurationVar(&o.duration, "d", 30*time.Second, "how long to send requests for")
	fs.Float64Var(&o.rate, "rate", 0, "target requests per second across all workers; 0 sends as fast as they can")
	fs.BoolVar(&o.stream, "stream", false, "request streamed responses")
	fs.IntVar(&o.maxTokens, "max-tokens", 20, "max_tokens to request; 0 leaves it out")
	fs.StringVar(&o.prompt, "prompt", "Say hello to the gateway.", "the user message")
	fs.BoolVar(&o.json, "json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if o.concurrency < 1 || o.duration <= 0 || o.rate < 0 {
		return errors.New("-c and -d must be positive, and -rate not negative")
	}

	rep, err := load(ctx, o)
	if err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	rep.print(stdout)
	return nil
}

// sample is one request's outcome.
type sample struct {
	outcome string        // "ok", an HTTP status, "error event" or "transport"
	total   time.Duration // send to the end of the response
	ttft    time.Duration // send to the first content; for a non-streamed response, the whole response
}

// load runs the workers for o.duration and returns the report.
func load(ctx context.Context, o options) (*report, error) {
	body, err := requestBody(o)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        o.concurrency,
		MaxIdleConnsPerHost: o.concurrency, // one kept-alive connection per worker
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}}
	defer client.CloseIdleConnections()

	// A pacer hands out one ticket per request when there is a target
	// rate. Without one, tickets is nil and workers never wait.
	stopAt := time.Now().Add(o.duration)
	ctx, cancel := context.WithDeadline(ctx, stopAt)
	defer cancel()
	var tickets chan struct{}
	if o.rate > 0 {
		tickets = make(chan struct{})
		go pace(ctx, o.rate, tickets)
	}

	start := time.Now()
	results := make([][]sample, o.concurrency)
	var wg sync.WaitGroup
	for i := range o.concurrency {
		wg.Go(func() {
			for {
				if tickets != nil {
					select {
					case <-tickets:
					case <-ctx.Done():
						return
					}
				}
				if ctx.Err() != nil {
					return
				}
				// A request started before the deadline runs to its end.
				results[i] = append(results[i], send(context.WithoutCancel(ctx), client, o, body))
			}
		})
	}
	wg.Wait()
	return summarise(o, slices.Concat(results...), time.Since(start)), nil
}

// pace sends a ticket every 1/rate seconds until ctx is done.
func pace(ctx context.Context, rate float64, tickets chan<- struct{}) {
	t := time.NewTicker(time.Duration(float64(time.Second) / rate))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case tickets <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func requestBody(o options) ([]byte, error) {
	req := map[string]any{
		"model":    o.model,
		"messages": []map[string]string{{"role": "user", "content": o.prompt}},
	}
	if o.maxTokens > 0 {
		req["max_tokens"] = o.maxTokens
	}
	if o.stream {
		req["stream"] = true
	}
	return json.Marshal(req)
}

// requestTimeout bounds one request, so a hung upstream can't hang the run.
const requestTimeout = 5 * time.Minute

// send makes one request and times it.
func send(ctx context.Context, client *http.Client, o options, body []byte) sample {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(o.url, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return sample{outcome: "transport"}
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{outcome: "transport", total: time.Since(start)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return sample{outcome: fmt.Sprint(resp.StatusCode), total: time.Since(start)}
	}
	if !o.stream {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return sample{outcome: "transport", total: time.Since(start)}
		}
		d := time.Since(start)
		return sample{outcome: "ok", total: d, ttft: d}
	}

	// A stream: the first event with content marks the first token.
	s := sample{outcome: "ok"}
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		data, isData := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if isData {
			switch {
			case data == "[DONE]":
				s.total = time.Since(start)
				return s
			case s.ttft == 0 && strings.Contains(data, `"content":"`) && !strings.Contains(data, `"content":""`):
				s.ttft = time.Since(start)
			case strings.HasPrefix(data, `{"error"`):
				s.outcome = "error event"
			}
		}
		if err != nil {
			// A stream that ends without [DONE] didn't complete.
			if s.outcome == "ok" {
				s.outcome = "transport"
			}
			s.total = time.Since(start)
			return s
		}
	}
}

// report is what a run measured.
type report struct {
	URL         string         `json:"url"`
	Model       string         `json:"model"`
	Stream      bool           `json:"stream"`
	Concurrency int            `json:"concurrency"`
	Rate        float64        `json:"target_rate,omitempty"`
	Requests    int            `json:"requests"`
	Seconds     float64        `json:"seconds"`
	PerSecond   float64        `json:"requests_per_second"`
	Errors      map[string]int `json:"errors"`
	Latency     percentiles    `json:"latency_ms"`
	TTFT        *percentiles   `json:"ttft_ms,omitempty"`
}

// percentiles are in milliseconds, over successful requests only.
type percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

func summarise(o options, samples []sample, elapsed time.Duration) *report {
	rep := &report{
		URL: o.url, Model: o.model, Stream: o.stream, Concurrency: o.concurrency, Rate: o.rate,
		Requests: len(samples), Seconds: elapsed.Seconds(), Errors: map[string]int{},
	}
	if elapsed > 0 {
		rep.PerSecond = float64(len(samples)) / elapsed.Seconds()
	}
	var totals, ttfts []time.Duration
	for _, s := range samples {
		if s.outcome != "ok" {
			rep.Errors[s.outcome]++
			continue
		}
		totals = append(totals, s.total)
		ttfts = append(ttfts, s.ttft)
	}
	rep.Latency = percentilesOf(totals)
	if o.stream {
		p := percentilesOf(ttfts)
		rep.TTFT = &p
	}
	return rep
}

// percentilesOf returns the 50th, 95th and 99th percentiles and the
// maximum, in milliseconds, by the nearest-rank method: the p-th
// percentile is the smallest value with at least p% of the values at or
// below it. It returns zeros for no values.
func percentilesOf(ds []time.Duration) percentiles {
	if len(ds) == 0 {
		return percentiles{}
	}
	slices.Sort(ds)
	at := func(p float64) float64 {
		// The rank is ceil(p% of n), counting from 1; the index is one less.
		i := int(math.Ceil(p/100*float64(len(ds)))) - 1
		return ms(ds[max(i, 0)])
	}
	return percentiles{P50: at(50), P95: at(95), P99: at(99), Max: ms(ds[len(ds)-1])}
}

// ms converts a duration to milliseconds, to the microsecond.
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func (r *report) print(w io.Writer) {
	mode := "non-streaming"
	if r.Stream {
		mode = "streaming"
	}
	fmt.Fprintf(w, "target    %s (model %s, %s, %d workers", r.URL, r.Model, mode, r.Concurrency)
	if r.Rate > 0 {
		fmt.Fprintf(w, ", target %.0f/s", r.Rate)
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintf(w, "requests  %d in %.1fs: %.1f/s\n", r.Requests, r.Seconds, r.PerSecond)
	if len(r.Errors) == 0 {
		fmt.Fprintln(w, "errors    none")
	} else {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(r.Errors)) {
			parts = append(parts, fmt.Sprintf("%s: %d", k, r.Errors[k]))
		}
		fmt.Fprintf(w, "errors    %s\n", strings.Join(parts, ", "))
	}
	printPercentiles(w, "latency", r.Latency)
	if r.TTFT != nil {
		printPercentiles(w, "ttft", *r.TTFT)
	}
}

func printPercentiles(w io.Writer, name string, p percentiles) {
	fmt.Fprintf(w, "%-9s p50 %.1fms  p95 %.1fms  p99 %.1fms  max %.1fms\n", name, p.P50, p.P95, p.P99, p.Max)
}
