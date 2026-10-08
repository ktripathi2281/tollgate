package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SSEWriter writes server-sent events to an HTTP response. Each event is
// flushed as soon as it is written, so the client receives it at once
// rather than when a buffer happens to fill.
type SSEWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

// NewSSEWriter returns an SSEWriter for w. The caller sets the response
// headers and status before the first event.
func NewSSEWriter(w http.ResponseWriter) *SSEWriter {
	return &SSEWriter{w: w, rc: http.NewResponseController(w)}
}

// WriteEvent writes one event whose data is data, in the form
// "data: <data>\n\n", and flushes it. data must not contain a newline,
// because a newline inside the data would end the line early and corrupt
// the event; JSON from encoding/json never contains one.
func (s *SSEWriter) WriteEvent(data []byte) error {
	// EXERCISE: implement WriteEvent; sse_test.go has every case it must pass.
	// Hint: reject data containing '\n' first. Then write "data: ", data and "\n\n"
	// (one Write call is simplest) and flush with s.rc.Flush(), returning either error.
	_ = data
	return errors.New("WriteEvent is not implemented yet")
}

// WriteJSON writes v, encoded as JSON, as one event.
func (s *SSEWriter) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	return s.WriteEvent(data)
}

// WriteDone writes the "[DONE]" event that ends an OpenAI stream.
func (s *SSEWriter) WriteDone() error {
	return s.WriteEvent([]byte("[DONE]"))
}

// SetWriteDeadline bounds how long the following writes may block on a
// client that has stopped reading. The zero time removes the deadline.
// Writers that don't support deadlines, such as httptest.ResponseRecorder,
// are left without one.
func (s *SSEWriter) SetWriteDeadline(t time.Time) error {
	if err := s.rc.SetWriteDeadline(t); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
