package sse

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// readAll reads every event and the error that ended the stream.
func readAll(r *Reader) ([]Event, error) {
	var events []Event
	for {
		ev, err := r.Next()
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

func TestReader(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []Event
		wantErr error
	}{
		{
			name:    "data events",
			input:   "data: {\"a\":1}\n\ndata: [DONE]\n\n",
			want:    []Event{{Data: `{"a":1}`}, {Data: "[DONE]"}},
			wantErr: io.EOF,
		},
		{
			name:    "named events",
			input:   "event: ping\ndata: {\"type\": \"ping\"}\n\nevent: message_stop\ndata: {}\n\n",
			want:    []Event{{Type: "ping", Data: `{"type": "ping"}`}, {Type: "message_stop", Data: "{}"}},
			wantErr: io.EOF,
		},
		{
			name:    "multi-line data is joined with newlines",
			input:   "data: one\ndata: two\n\n",
			want:    []Event{{Data: "one\ntwo"}},
			wantErr: io.EOF,
		},
		{
			name:    "the space after the colon is optional",
			input:   "data:tight\n\ndata:  two spaces\n\n",
			want:    []Event{{Data: "tight"}, {Data: " two spaces"}},
			wantErr: io.EOF,
		},
		{
			name:    "CRLF line endings",
			input:   "event: x\r\ndata: y\r\n\r\n",
			want:    []Event{{Type: "x", Data: "y"}},
			wantErr: io.EOF,
		},
		{
			name:    "comments, extra blank lines and unknown fields are ignored",
			input:   ": keepalive\n\n\nid: 7\nretry: 100\ndata: x\n\n",
			want:    []Event{{Data: "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "an event without data is skipped",
			input:   "event: empty\n\ndata: x\n\n",
			want:    []Event{{Data: "x"}},
			wantErr: io.EOF,
		},
		{
			name:    "empty stream",
			input:   "",
			wantErr: io.EOF,
		},
		{
			name:    "cut off before the blank line",
			input:   "data: x\n\ndata: [DONE]\n",
			want:    []Event{{Data: "x"}},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "cut off mid-line",
			input:   "data: {\"cho",
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "a comment at the end is not an unfinished event",
			input:   "data: x\n\n: bye\n",
			want:    []Event{{Data: "x"}},
			wantErr: io.EOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// One byte at a time, as a slow network would deliver it.
			got, err := readAll(NewReader(iotest.OneByteReader(strings.NewReader(tt.input))))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("events = %+v, want %+v", got, tt.want)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ended with %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestReaderLongLines(t *testing.T) {
	long := strings.Repeat("x", 100<<10) // longer than the buffer, under the cap
	events, err := readAll(NewReader(strings.NewReader("data: " + long + "\n\n")))
	if !errors.Is(err, io.EOF) || len(events) != 1 || events[0].Data != long {
		t.Errorf("a 100 KiB line: got %d events, %v", len(events), err)
	}

	tooLong := strings.Repeat("x", maxLine+1)
	if _, err := NewReader(strings.NewReader("data: " + tooLong + "\n\n")).Next(); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("a line over the cap: got %v, want ErrLineTooLong", err)
	}
}

func TestReaderPassesReadErrorsThrough(t *testing.T) {
	broken := errors.New("connection reset")
	r := NewReader(io.MultiReader(strings.NewReader("data: x\n\n"), iotest.ErrReader(broken)))
	events, err := readAll(r)
	if len(events) != 1 || !errors.Is(err, broken) {
		t.Errorf("got %d events and %v, want 1 and the read error", len(events), err)
	}
}
