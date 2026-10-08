// Package sse reads server-sent events: the format OpenAI and Anthropic
// use to stream responses.
package sse

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Event is one server-sent event.
type Event struct {
	// Type is the event's "event:" field, or "" if it had none.
	Type string
	// Data is the event's "data:" lines, joined with newlines.
	Data string
}

// maxLine caps one line of the stream, so a broken upstream can't make the
// reader buffer without limit.
const maxLine = 1 << 20

// ErrLineTooLong is returned for a line longer than 1 MiB.
var ErrLineTooLong = errors.New("sse: line too long")

// Reader reads events from a stream.
type Reader struct {
	r *bufio.Reader
}

// NewReader returns a Reader for r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 32<<10)}
}

// Next returns the next event. It returns io.EOF when the stream ends
// between events, and io.ErrUnexpectedEOF when it ends partway through
// one, which means the upstream connection broke.
//
// Following the SSE spec, comment lines (starting with ':') and fields
// other than "event" and "data" are ignored, and an event with no data is
// not returned.
func (r *Reader) Next() (Event, error) {
	var (
		ev      Event
		data    []string
		pending bool // a field of the current event has been read
	)
	for {
		line, err := r.readLine()
		if errors.Is(err, io.EOF) {
			if pending {
				return Event{}, io.ErrUnexpectedEOF
			}
			return Event{}, io.EOF
		}
		if err != nil {
			return Event{}, err
		}

		if line == "" {
			// A blank line ends the event.
			if len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			ev, pending = Event{}, false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // a comment, often used as a keepalive
		}

		pending = true
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			ev.Type = value
		}
	}
}

// readLine returns the next line without its line ending (\n or \r\n). A
// last line without a line ending means the stream was cut off, so it is
// reported as io.ErrUnexpectedEOF.
func (r *Reader) readLine() (string, error) {
	var line []byte
	for {
		part, err := r.r.ReadSlice('\n')
		if len(line)+len(part) > maxLine {
			return "", ErrLineTooLong
		}
		line = append(line, part...)
		switch {
		case err == nil:
			s := strings.TrimSuffix(string(line), "\n")
			return strings.TrimSuffix(s, "\r"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue // the line is longer than the buffer; keep reading
		case errors.Is(err, io.EOF) && len(line) > 0:
			return "", io.ErrUnexpectedEOF
		default:
			return "", err
		}
	}
}
