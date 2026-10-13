package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestPercentiles(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	// Shuffle-ish: percentilesOf must sort.
	ds[0], ds[99] = ds[99], ds[0]
	got := percentilesOf(ds)
	if got != (percentiles{P50: 50, P95: 95, P99: 99, Max: 100}) {
		t.Errorf("percentilesOf(1..100ms) = %+v", got)
	}
	if one := percentilesOf([]time.Duration{7 * time.Millisecond}); one != (percentiles{7, 7, 7, 7}) {
		t.Errorf("one sample: %+v", one)
	}
	if none := percentilesOf(nil); none != (percentiles{}) {
		t.Errorf("no samples: %+v", none)
	}
}

// fakeAPI answers chat completions like the gateway: after a short delay,
// a JSON reply, or a stream whose first content arrives after ttft. Every
// fifth request is refused with 429.
func fakeAPI(t *testing.T, ttft time.Duration) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var calls, authed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer tg_test" {
			authed.Add(1)
		}
		body, _ := io.ReadAll(r.Body)
		if n%5 == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			time.Sleep(ttft)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		flusher.Flush()
		time.Sleep(ttft)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		time.Sleep(ttft)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &authed
}

func TestLoadNonStreaming(t *testing.T) {
	srv, calls, authed := fakeAPI(t, 5*time.Millisecond)
	rep, err := load(t.Context(), options{
		url: srv.URL + "/v1", key: "tg_test", model: "m", concurrency: 4, duration: 200 * time.Millisecond, maxTokens: 5, prompt: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Requests == 0 || int64(rep.Requests) != calls.Load() || authed.Load() != calls.Load() {
		t.Fatalf("requests %d, server saw %d (%d with the key)", rep.Requests, calls.Load(), authed.Load())
	}
	if rep.Errors["429"] != rep.Requests/5 {
		t.Errorf("errors = %v, want every fifth request counted as 429", rep.Errors)
	}
	if rep.Latency.P50 < 5 || rep.TTFT != nil || rep.PerSecond <= 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestLoadStreaming(t *testing.T) {
	srv, _, _ := fakeAPI(t, 10*time.Millisecond)
	rep, err := load(t.Context(), options{url: srv.URL + "/v1", model: "m", concurrency: 2, duration: 150 * time.Millisecond, stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.TTFT == nil {
		t.Fatal("no time to first token for a streaming run")
	}
	// The first content arrives after one delay; the stream ends after two.
	// The empty role event before it doesn't count as content.
	if rep.TTFT.P50 < 10 || rep.Latency.P50 < 20 || rep.TTFT.P50 >= rep.Latency.P50 {
		t.Errorf("ttft %+v, latency %+v", *rep.TTFT, rep.Latency)
	}
}

func TestLoadAtATargetRate(t *testing.T) {
	srv, calls, _ := fakeAPI(t, 0)
	rep, err := load(t.Context(), options{url: srv.URL + "/v1", model: "m", concurrency: 8, duration: 500 * time.Millisecond, rate: 40})
	if err != nil {
		t.Fatal(err)
	}
	// 40 a second for half a second is 20; the ticker allows a little slack.
	if rep.Requests < 15 || rep.Requests > 21 || calls.Load() != int64(rep.Requests) {
		t.Errorf("%d requests at 40/s for 0.5s, want about 20", rep.Requests)
	}
}

func TestReport(t *testing.T) {
	rep := summarise(options{url: "http://x/v1", model: "m", concurrency: 2, stream: true}, []sample{
		{outcome: "ok", total: 10 * time.Millisecond, ttft: 4 * time.Millisecond},
		{outcome: "ok", total: 20 * time.Millisecond, ttft: 6 * time.Millisecond},
		{outcome: "429", total: time.Millisecond},
		{outcome: "error event", total: 5 * time.Millisecond},
	}, 2*time.Second)
	var text bytes.Buffer
	rep.print(&text)
	for _, want := range []string{"4 in 2.0s: 2.0/s", "errors    429: 1, error event: 1", "latency   p50 10.0ms", "ttft      p50 4.0ms"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, text.String())
		}
	}
	js, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(js), `"requests_per_second":2`) || !strings.Contains(string(js), `"ttft_ms":{"p50":4`) {
		t.Errorf("JSON report = %s, %v", js, err)
	}
}

func TestRunFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"-c", "0"}, &out, &errOut); err == nil {
		t.Error("-c 0 must be rejected")
	}
	if err := run(t.Context(), []string{"extra"}, &out, &errOut); err == nil {
		t.Error("extra arguments must be rejected")
	}
	if err := run(t.Context(), []string{"-h"}, &out, &errOut); err != nil || !strings.Contains(errOut.String(), "-stream") {
		t.Errorf("-h: %v, %q", err, errOut.String())
	}
}
