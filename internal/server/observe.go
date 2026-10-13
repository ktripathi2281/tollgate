package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/metrics"
	"github.com/ktripathi2281/tollgate/internal/router"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// RequestLog takes one row per chat request. usage.Recorder is the
// implementation; Record must not block.
type RequestLog interface {
	Record(e usage.Entry) bool
}

// noRequestLog drops every row, for a server built without a request log.
type noRequestLog struct{}

func (noRequestLog) Record(usage.Entry) bool { return false }

// requestInfo is what the server learns about a chat request as it runs:
// the request-log row, and the time spent upstream, for the overhead
// metric. The access-log middleware creates it, the middleware and
// handlers inside fill it in, and the access log records it once the
// response is done. All of that happens on the request's goroutine.
type requestInfo struct {
	usage.Entry
	upstream time.Duration
}

// infoFrom returns the request's requestInfo, or a throwaway one for
// requests that don't get a row, so callers never check for nil.
func infoFrom(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(requestInfoKey).(*requestInfo); ok {
		return info
	}
	return &requestInfo{}
}

// writeError sends an error response and notes its code for the request
// log.
func writeError(w http.ResponseWriter, r *http.Request, e *api.Error) {
	infoFrom(r.Context()).ErrorCode = e.Code
	api.WriteError(w, e)
}

// chatPath is the one route whose requests get a request-log row.
const chatPath = "/v1/chat/completions"

// routeOf returns the route label for a request's path. Unknown paths share
// one label, so a client probing paths can't create new series.
func routeOf(path string) string {
	switch path {
	case chatPath, "/v1/models", "/healthz", "/readyz":
		return path
	}
	if strings.HasPrefix(path, "/v1/") {
		return "/v1/other"
	}
	return "other"
}

// recordRouting notes which target served the request, or was tried last,
// and how many calls it took, and records the attempts in the metrics.
func (s *Server) recordRouting(info *requestInfo, alias string, res router.Result) {
	info.Provider, info.Model, info.Attempts = res.Provider, res.Model, res.Attempts
	s.observeTries(info, alias, res.Tries)
}

// observeTries records the router's attempts: each call or skip by
// provider and outcome, retries of the same target, and fallbacks to the
// next. It also adds their time to the request's upstream time.
func (s *Server) observeTries(info *requestInfo, alias string, tries []router.Try) {
	for i, t := range tries {
		providerName, _, _ := strings.Cut(t.Target, "/")
		s.metrics.Upstream(providerName, t.Outcome)
		info.upstream += time.Duration(t.DurationMS * float64(time.Millisecond))
		if i == 0 {
			continue
		}
		if t.Target == tries[i-1].Target {
			s.metrics.Retry(providerName)
		} else {
			s.metrics.Fallback(alias)
		}
	}
}

// observeChat records what a finished chat request contributes to the
// metrics.
func (s *Server) observeChat(info *requestInfo) {
	e := &info.Entry
	if e.Provider != "" && (e.PromptTokens > 0 || e.CompletionTokens > 0) {
		s.metrics.Usage(e.Provider, e.PromptTokens, e.CompletionTokens, e.Cost)
	}
	if e.TTFT > 0 {
		s.metrics.TimeToFirstToken(e.Provider, e.TTFT)
	}
	// A stream's time upstream is most of its life, so overhead is only
	// measured for non-streaming calls.
	if !e.Stream && e.Attempts > 0 {
		s.metrics.Overhead(e.Latency - info.upstream)
	}
	if e.Replayed {
		s.metrics.Replay()
	}
}

// registerMetrics exports what the server itself can report at scrape
// time: requests in flight and each target's breaker.
func (s *Server) registerMetrics() {
	s.metrics.Gauge("inflight_requests", "API requests being handled now.",
		func() float64 { return float64(len(s.inflight)) })
	s.metrics.Breakers(func() []metrics.BreakerState {
		var out []metrics.BreakerState
		for _, b := range s.router.BreakerStates() {
			out = append(out, metrics.BreakerState{Alias: b.Alias, Target: b.Target, State: b.State})
		}
		return out
	})
}
