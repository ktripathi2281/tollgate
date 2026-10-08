package api

import (
	"bytes"
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
	if bytes.IndexByte(data, '\n') >= 0 {
		return errors.New("event data must not contain a newline")
	}
	// Build the whole event first so it goes out in one Write: a client
	// never sees half an event, even if the connection fails mid-write.
	event := make([]byte, 0, len(data)+len("data: \n\n"))
	event = append(event, "data: "...)
	event = append(event, data...)
	event = append(event, "\n\n"...)
	if _, err := s.w.Write(event); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	// The event sits in net/http's buffer until it is flushed.
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush event: %w", err)
	}
	return nil
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
