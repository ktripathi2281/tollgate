package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// These tests are for the M2 exercise, SSEWriter.WriteEvent.

// flushRecorder is a ResponseWriter that records how much of the body had
// been written each time it was flushed.
type flushRecorder struct {
	header  http.Header
	body    []byte
	flushes []int
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}
func (f *flushRecorder) Write(b []byte) (int, error) {
	f.body = append(f.body, b...)
	return len(b), nil
}
func (f *flushRecorder) Flush() { f.flushes = append(f.flushes, len(f.body)) }

// noFlushWriter is a ResponseWriter that can't flush.
type noFlushWriter struct{ header http.Header }

func (n *noFlushWriter) Header() http.Header         { return n.header }
func (n *noFlushWriter) WriteHeader(int)             {}
func (n *noFlushWriter) Write(b []byte) (int, error) { return len(b), nil }

// brokenWriter is a ResponseWriter whose client has gone away.
type brokenWriter struct{ flushRecorder }

var errBrokenPipe = errors.New("broken pipe")

func (b *brokenWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

func TestWriteEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := NewSSEWriter(rec).WriteEvent([]byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if got, want := rec.Body.String(), "data: {\"a\":1}\n\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("the event was not flushed")
	}
}

func TestWriteEventFlushesEachEvent(t *testing.T) {
	f := &flushRecorder{header: http.Header{}}
	s := NewSSEWriter(f)
	for _, data := range []string{`{"n":1}`, `{"n":2}`} {
		if err := s.WriteEvent([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	first := len("data: {\"n\":1}\n\n")
	if want := []int{first, 2 * first}; !slices.Equal(f.flushes, want) {
		t.Errorf("flushed after %v bytes, want after each whole event: %v", f.flushes, want)
	}
	if got, want := string(f.body), "data: {\"n\":1}\n\ndata: {\"n\":2}\n\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestWriteEventRejectsNewlines(t *testing.T) {
	f := &flushRecorder{header: http.Header{}}
	if err := NewSSEWriter(f).WriteEvent([]byte("line one\nline two")); err == nil {
		t.Error("WriteEvent accepted data with a newline")
	}
	if len(f.body) != 0 || len(f.flushes) != 0 {
		t.Errorf("wrote %q and flushed %d times, want nothing written", f.body, len(f.flushes))
	}
}

func TestWriteEventReportsWriteErrors(t *testing.T) {
	b := &brokenWriter{flushRecorder{header: http.Header{}}}
	if err := NewSSEWriter(b).WriteEvent([]byte("{}")); !errors.Is(err, errBrokenPipe) {
		t.Errorf("got %v, want the write error", err)
	}
}

func TestWriteEventNeedsAFlusher(t *testing.T) {
	err := NewSSEWriter(&noFlushWriter{header: http.Header{}}).WriteEvent([]byte("{}"))
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("got %v, want an error wrapping http.ErrNotSupported", err)
	}
}

func TestWriteJSONAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewSSEWriter(rec)
	if err := s.WriteJSON(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteDone(); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Body.String(), "data: {\"n\":1}\n\ndata: [DONE]\n\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestSetWriteDeadlineIgnoresUnsupportedWriters(t *testing.T) {
	// httptest.ResponseRecorder has no connection, so no deadlines.
	if err := NewSSEWriter(httptest.NewRecorder()).SetWriteDeadline(fixedNow()); err != nil {
		t.Errorf("SetWriteDeadline = %v, want nil", err)
	}
}
