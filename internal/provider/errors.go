package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Class groups upstream failures by what the router should do about them.
type Class int

// The failure classes from brief section 6.
const (
	// Unavailable: 5xx, 529, connection errors, timeouts, error events in a
	// stream. Retry, then fall back. Counts toward the circuit breaker.
	Unavailable Class = iota + 1
	// RateLimited: 429. Honour a short Retry-After, otherwise fall back.
	// Doesn't trip the breaker.
	RateLimited
	// BadRequest: any other 4xx. Return it to the client; no retry, no
	// fallback.
	BadRequest
	// AuthOrConfig: 401 or 403 from upstream, meaning the gateway's own key
	// or config is wrong. Fall back and log loudly; never show the client
	// the upstream detail.
	AuthOrConfig
	// Cancelled: the client went away. Stop everything; it isn't a failure.
	Cancelled
)

func (c Class) String() string {
	switch c {
	case Unavailable:
		return "unavailable"
	case RateLimited:
		return "rate_limited"
	case BadRequest:
		return "bad_request"
	case AuthOrConfig:
		return "auth_or_config"
	case Cancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("Class(%d)", int(c))
	}
}

// Error is a classified upstream failure.
type Error struct {
	Provider string
	Class    Class
	// Status is the upstream HTTP status, or 0 if there was no response.
	Status int
	// Message describes the failure. For BadRequest it is meant for the
	// client; for other classes it is only logged.
	Message string
	// RetryAfter is the upstream's Retry-After hint, or 0 if it sent none.
	RetryAfter time.Duration
	// Err is the underlying cause, if any.
	Err error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Provider, e.Class)
	if e.Status != 0 {
		msg += fmt.Sprintf(" (status %d)", e.Status)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ClassifyStatus maps an upstream HTTP error status to a Class.
func ClassifyStatus(status int) Class {
	switch {
	case status == http.StatusTooManyRequests:
		return RateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return AuthOrConfig
	case status >= 400 && status < 500:
		return BadRequest
	default:
		// 5xx, Anthropic's 529, and anything unexpected.
		return Unavailable
	}
}

// ClassOf returns the class of an error returned by a Provider. A cancelled
// context is Cancelled and an expired deadline is Unavailable (a timeout),
// whether or not an adapter wrapped them. Any other unclassified error, such
// as a connection failure, is Unavailable.
func ClassOf(err error) Class {
	if errors.Is(err, context.Canceled) {
		return Cancelled
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class
	}
	return Unavailable
}
