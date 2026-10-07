package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		status int
		want   Class
	}{
		{400, BadRequest},
		{401, AuthOrConfig},
		{403, AuthOrConfig},
		{404, BadRequest},
		{413, BadRequest},
		{422, BadRequest},
		{429, RateLimited},
		{500, Unavailable},
		{502, Unavailable},
		{503, Unavailable},
		{504, Unavailable},
		{529, Unavailable}, // Anthropic's "overloaded"
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			if got := ClassifyStatus(tt.status); got != tt.want {
				t.Errorf("ClassifyStatus(%d) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestClassOf(t *testing.T) {
	rateLimited := &Error{Provider: "p", Class: RateLimited, Status: 429}
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"classified error", rateLimited, RateLimited},
		{"wrapped classified error", fmt.Errorf("call: %w", rateLimited), RateLimited},
		{"cancelled context", context.Canceled, Cancelled},
		{"wrapped cancelled context", fmt.Errorf("read: %w", context.Canceled), Cancelled},
		{"cancellation wins over a class", &Error{Class: Unavailable, Err: context.Canceled}, Cancelled},
		{"deadline is a timeout", context.DeadlineExceeded, Unavailable},
		{"unknown error", errors.New("connection reset"), Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassOf(tt.err); got != tt.want {
				t.Errorf("ClassOf(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	err := &Error{
		Provider: "mock", Class: Unavailable, Status: 503,
		Message: "overloaded", Err: errors.New("boom"),
	}
	want := "mock: unavailable (status 503): overloaded: boom"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
