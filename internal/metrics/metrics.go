// Package metrics defines the gateway's Prometheus metrics (brief section
// 10). They are served on the admin listener. No metric is labelled by API
// key or request ID: labels hold only values from the config or small
// fixed sets, so the number of series stays bounded.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ktripathi2281/tollgate/internal/money"
)

const namespace = "tollgate"

// Rejection reasons, for Rejected.
const (
	RequestsPerMinute = "requests_per_minute"
	TokensPerMinute   = "tokens_per_minute"
	Budget            = "budget"
)

// Metrics holds the gateway's metrics and the registry that serves them.
// It is safe for concurrent use.
type Metrics struct {
	reg *prometheus.Registry

	httpDuration *prometheus.HistogramVec
	overhead     prometheus.Histogram
	ttft         *prometheus.HistogramVec
	upstream     *prometheus.CounterVec
	retries      *prometheus.CounterVec
	fallbacks    *prometheus.CounterVec
	rejections   *prometheus.CounterVec
	tokens       *prometheus.CounterVec
	cost         *prometheus.CounterVec
	replays      prometheus.Counter
}

// New returns the metrics, registered with a registry of their own along
// with the Go runtime and process metrics.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{
		reg: reg,
		// The histogram's _count series is the request count by route and
		// status, so there is no separate counter.
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "HTTP requests by route and status, and how long they took.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"route", "status"}),
		overhead: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "gateway_overhead_seconds",
			Help:    "Time a non-streaming chat request spent in the gateway rather than upstream.",
			Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25},
		}),
		ttft: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "time_to_first_token_seconds",
			Help:    "Time from a streaming request's arrival to its first content.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
		}, []string{"provider"}),
		upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "upstream_requests_total",
			Help: "Upstream calls by provider and outcome: ok, an error class, or skipped by an open breaker.",
		}, []string{"provider", "outcome"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "upstream_retries_total",
			Help: "Calls that retried the same target after a failure.",
		}, []string{"provider"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "upstream_fallbacks_total",
			Help: "Moves from one of an alias's targets to the next.",
		}, []string{"alias"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "rejections_total",
			Help: "Requests refused by a rate limit or a budget.",
		}, []string{"reason"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "tokens_total",
			Help: "Tokens used, by provider and kind (prompt or completion).",
		}, []string{"provider", "kind"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "cost_usd_total",
			Help: "What requests cost, by provider. The budget tables hold the exact amounts.",
		}, []string{"provider"}),
		replays: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "idempotent_replays_total",
			Help: "Responses replayed from an idempotency key.",
		}),
	}
	reg.MustRegister(m.httpDuration, m.overhead, m.ttft, m.upstream, m.retries, m.fallbacks,
		m.rejections, m.tokens, m.cost, m.replays)
	return m
}

// Handler serves the metrics in Prometheus's format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// HTTPRequest records one HTTP request on a route.
func (m *Metrics) HTTPRequest(route string, status int, d time.Duration) {
	m.httpDuration.WithLabelValues(route, strconv.Itoa(status)).Observe(d.Seconds())
}

// Overhead records the time a request spent in the gateway itself.
func (m *Metrics) Overhead(d time.Duration) {
	m.overhead.Observe(max(d, 0).Seconds())
}

// TimeToFirstToken records how long a stream took to send content.
func (m *Metrics) TimeToFirstToken(provider string, d time.Duration) {
	m.ttft.WithLabelValues(provider).Observe(d.Seconds())
}

// Upstream records one upstream call, or a target skipped.
func (m *Metrics) Upstream(provider, outcome string) {
	m.upstream.WithLabelValues(provider, outcome).Inc()
}

// Retry records a retry of the same target.
func (m *Metrics) Retry(provider string) {
	m.retries.WithLabelValues(provider).Inc()
}

// Fallback records a move to an alias's next target.
func (m *Metrics) Fallback(alias string) {
	m.fallbacks.WithLabelValues(alias).Inc()
}

// Rejected records a request refused for one of the rejection reasons.
func (m *Metrics) Rejected(reason string) {
	m.rejections.WithLabelValues(reason).Inc()
}

// Usage records the tokens a request used and what they cost.
func (m *Metrics) Usage(provider string, prompt, completion int, cost money.Micros) {
	m.tokens.WithLabelValues(provider, "prompt").Add(float64(prompt))
	m.tokens.WithLabelValues(provider, "completion").Add(float64(completion))
	m.cost.WithLabelValues(provider).Add(float64(cost) / float64(money.PerUSD))
}

// Replay records a response replayed from an idempotency key.
func (m *Metrics) Replay() {
	m.replays.Inc()
}

// Gauge exports a value read at scrape time, such as the requests in
// flight.
func (m *Metrics) Gauge(name, help string, value func() float64) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, value))
}

// Counter exports a count kept elsewhere, such as an atomic counter, read
// at scrape time.
func (m *Metrics) Counter(name, help string, value func() float64) {
	m.reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, value))
}

// BreakerState is one target's circuit-breaker state.
type BreakerState struct {
	Alias, Target, State string
}

// breakerStates are the values of the state label.
var breakerStates = []string{"closed", "half_open", "open"}

// Breakers exports every target's breaker state, read at scrape time:
// tollgate_breaker_state{alias, target, state} is 1 for the current state
// and 0 for the others.
func (m *Metrics) Breakers(states func() []BreakerState) {
	m.reg.MustRegister(&breakerCollector{states: states, desc: prometheus.NewDesc(
		namespace+"_breaker_state",
		"Each target's circuit-breaker state: 1 for the current one.",
		[]string{"alias", "target", "state"}, nil,
	)})
}

type breakerCollector struct {
	states func() []BreakerState
	desc   *prometheus.Desc
}

func (c *breakerCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *breakerCollector) Collect(ch chan<- prometheus.Metric) {
	for _, b := range c.states() {
		for _, s := range breakerStates {
			v := 0.0
			if s == b.State {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, b.Alias, b.Target, s)
		}
	}
}
