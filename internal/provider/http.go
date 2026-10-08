package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NewHTTPClient returns the client an HTTP adapter uses for all its calls.
// Its transport settings are explicit, and it has no overall Timeout:
// every call is bounded by its context's deadlines instead, so long
// streams aren't cut off.
func NewHTTPClient(connectTimeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: connectTimeout,
		// Each adapter talks to one host, so the per-host cap is the one
		// that matters. The default of 2 idle connections would make a busy
		// gateway open and close connections constantly.
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   512,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

// RetryAfter reads a Retry-After header, given either as a number of
// seconds or as an HTTP date. It returns 0 if the header is absent,
// unreadable or in the past.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 64 << 10

// ReadErrorBody reads up to 64 KiB of an error response body: enough for
// any provider's error JSON, without trusting the upstream's size.
func ReadErrorBody(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	return b
}

// TransportError classifies a failure to talk to the upstream at all, such
// as a refused connection or a broken stream. If ctx has ended, the context
// error is wrapped so that ClassOf and the router's timeout handling see
// it, whatever error the transport reported.
func TransportError(ctx context.Context, name string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", name, ctx.Err())
	}
	return &Error{Provider: name, Class: Unavailable, Message: "request failed", Err: err}
}
