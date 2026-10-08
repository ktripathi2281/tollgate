// Package api holds the gateway's OpenAI-compatible wire format: request
// parsing and validation, response types, and the error envelope.
package api

import (
	"fmt"
	"time"
)

// Error types, using OpenAI's names.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeServer         = "server_error"
)

// Error codes. Where OpenAI has a code for the same case, the gateway uses
// it; the rest follow the same snake_case style.
const (
	CodeInvalidJSON          = "invalid_json"
	CodeMissingParameter     = "missing_required_parameter"
	CodeInvalidType          = "invalid_type"
	CodeInvalidValue         = "invalid_value"
	CodeUnknownParameter     = "unknown_parameter"
	CodeUnsupportedParameter = "unsupported_parameter"
	CodeUnsupportedValue     = "unsupported_value"
	CodeModelNotFound        = "model_not_found"
	CodeRequestTooLarge      = "request_too_large"
	CodeOverloaded           = "overloaded"
	CodeUpstreamError        = "upstream_error"
	CodeUpstreamTimeout      = "upstream_timeout"
	CodeUpstreamRateLimited  = "upstream_rate_limited"
	CodeShuttingDown         = "shutting_down"
	CodeInternal             = "internal_error"
)

// Error is an error in OpenAI's format, with the HTTP status to send it with.
type Error struct {
	Status  int
	Type    string
	Code    string // empty is sent as null
	Param   string // empty is sent as null
	Message string
	// RetryAfter, if positive, is sent as a Retry-After header (rounded up
	// to whole seconds). It is not part of the JSON body.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return e.Message
}

// Envelope is the JSON body of an error response:
// {"error": {"message", "type", "param", "code"}}.
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the object inside an Envelope. Param and Code are
// pointers so that an empty value is sent as JSON null, as OpenAI does.
type EnvelopeError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// Envelope returns the error's JSON body.
func (e *Error) Envelope() Envelope {
	return Envelope{Error: EnvelopeError{
		Message: e.Message,
		Type:    e.Type,
		Param:   nullIfEmpty(e.Param),
		Code:    nullIfEmpty(e.Code),
	}}
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// invalidf returns a 400 invalid_request_error for param.
func invalidf(param, code, format string, args ...any) *Error {
	return &Error{
		Status:  400,
		Type:    TypeInvalidRequest,
		Code:    code,
		Param:   param,
		Message: fmt.Sprintf(format, args...),
	}
}
