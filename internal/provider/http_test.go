package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{" 20 ", 20 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"soon", 0},
		{"Thu, 08 Oct 2026 12:00:30 GMT", 30 * time.Second},
		{"Thu, 08 Oct 2026 11:59:00 GMT", 0}, // in the past
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Retry-After", tt.header)
		}
		if got := RetryAfter(h, now); got != tt.want {
			t.Errorf("RetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestReadErrorBodyIsCapped(t *testing.T) {
	big := strings.NewReader(strings.Repeat("x", 1<<20))
	if got := len(ReadErrorBody(big)); got != maxErrorBody {
		t.Errorf("read %d bytes, want the cap of %d", got, maxErrorBody)
	}
}

func TestTransportError(t *testing.T) {
	refused := errors.New("connection refused")

	err := TransportError(t.Context(), "p", refused)
	if ClassOf(err) != Unavailable || !errors.Is(err, refused) {
		t.Errorf("got %v, want an Unavailable error wrapping the cause", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Whatever the transport says, a cancelled context means a cancellation.
	if err := TransportError(ctx, "p", refused); ClassOf(err) != Cancelled {
		t.Errorf("got %v (%v), want Cancelled", err, ClassOf(err))
	}
}
