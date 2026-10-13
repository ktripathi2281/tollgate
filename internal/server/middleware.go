package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// Response headers. The gateway's own headers share one prefix, so renaming
// the project changes one constant.
const (
	headerPrefix    = "X-Tollgate-"
	HeaderRequestID = "X-Request-Id"
	HeaderProvider  = headerPrefix + "Provider"
	HeaderAttempts  = headerPrefix + "Attempts"
)

// ctxKey is the type of this package's context keys. A private type means no
// other package can read or overwrite them by accident.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	apiKeyKey
	requestInfoKey
)

// RequestID returns the request ID stored in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withRequestID gives every request a fresh random ID, returns it in the
// X-Request-Id header, and stores it in the request context for logs.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "req_" + rand.Text()
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// responseRecorder wraps a ResponseWriter to remember the status and size of
// the response, for the access log.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *responseRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// streaming needs in order to flush.
func (rec *responseRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// logRequests writes one access log line per request, and records the
// request's duration by route and status. A chat request also gets a
// requestInfo, which the handlers fill in, and which becomes its
// request-log row and its chat metrics once the response is done. The
// access log holds only request metadata: never headers, bodies, prompts
// or completions.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var info *requestInfo
		if r.Method == http.MethodPost && r.URL.Path == chatPath {
			info = &requestInfo{Entry: usage.Entry{Time: start, RequestID: RequestID(r.Context())}}
			r = r.WithContext(context.WithValue(r.Context(), requestInfoKey, info))
		}
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK // net/http sends 200 when a handler writes nothing
		}
		elapsed := time.Since(start)
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug // probes would otherwise drown the log
		}
		s.log.LogAttrs(r.Context(), level, "request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
		)
		s.metrics.HTTPRequest(routeOf(r.URL.Path), status, elapsed)

		if info != nil {
			info.Status, info.Latency = status, elapsed
			s.observeChat(info)
			s.requests.Record(info.Entry) // never blocks: a full queue drops the row
		}
	})
}

// recoverPanics turns a panic in a handler into a 500 response and an error
// log with the stack, instead of a dropped connection.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// http.ErrAbortHandler is net/http's own way to abort a
			// response. It is not a bug, so let it through.
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			log.LogAttrs(r.Context(), slog.LevelError, "panic",
				slog.String("request_id", RequestID(r.Context())),
				slog.String("panic", fmt.Sprint(v)),
				slog.String("stack", string(debug.Stack())),
			)
			writeError(w, r, &api.Error{
				Status:  http.StatusInternalServerError,
				Type:    api.TypeServer,
				Code:    api.CodeInternal,
				Message: "The gateway hit an internal error.",
			})
		}()
		next.ServeHTTP(w, r)
	})
}

// limitInflight lets at most cap(s.inflight) requests through to next at
// once. The slots are a buffered channel used as a semaphore: sending takes
// a slot, receiving gives it back, and its length is the requests in
// flight. A request that finds no free slot is shed at once with 503,
// because queueing would only add latency under overload.
func (s *Server) limitInflight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.inflight <- struct{}{}:
			defer func() { <-s.inflight }()
			next.ServeHTTP(w, r)
		default:
			writeError(w, r, &api.Error{
				Status:     http.StatusServiceUnavailable,
				Type:       api.TypeServer,
				Code:       api.CodeOverloaded,
				Message:    "The gateway is handling too many requests. Retry shortly.",
				RetryAfter: time.Second,
			})
		}
	})
}
