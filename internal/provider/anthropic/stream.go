package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/sse"
)

// stream reads a Messages API event stream. Its context is the one the call
// was made with; cancelling it aborts the body read.
type stream struct {
	ctx    context.Context
	name   string
	body   io.ReadCloser
	events *sse.Reader

	input  int    // prompt tokens, from message_start
	output int    // output tokens so far: message_delta counts are cumulative
	stop   string // the stop reason, from message_delta
	done   bool
	closed bool
}

func newStream(ctx context.Context, name string, body io.ReadCloser) *stream {
	return &stream{ctx: ctx, name: name, body: body, events: sse.NewReader(body)}
}

// event is the part of any stream event the adapter reads. Which fields are
// set depends on Type.
type event struct {
	Type    string `json:"type"`
	Message *struct {
		Model string `json:"model"`
		Usage usage  `json:"usage"`
	} `json:"message"` // message_start
	ContentBlock *struct {
		Type string `json:"type"`
	} `json:"content_block"` // content_block_start
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"` // content_block_delta and message_delta
	Usage *usage `json:"usage"` // message_delta
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"` // error
}

// keepalive is a chunk with nothing for the client: a ping, a block
// boundary, or an event the gateway doesn't model.
var keepalive = provider.Chunk{}

// Next translates the next event:
//
//   - message_start gives the model and the prompt tokens;
//   - text_delta becomes text;
//   - thinking becomes a reasoning chunk: the model is working, but there
//     is nothing for the client yet;
//   - message_delta carries the stop reason and the output tokens so far;
//   - message_stop becomes the final chunk, with the finish reason and usage;
//   - an error event ends the stream with an error;
//   - pings, block boundaries, other delta types and unknown event types
//     are keepalives, as Anthropic's versioning policy asks.
func (s *stream) Next() (*provider.Chunk, error) {
	if s.done {
		return nil, io.EOF
	}
	ev, err := s.events.Next()
	if err != nil {
		s.done = true
		switch {
		case errors.Is(err, io.EOF):
			// Must not wrap io.EOF: that would read as a complete stream.
			err = errStreamEnded
		case errors.Is(err, io.ErrUnexpectedEOF):
			err = fmt.Errorf("%w: %w", errStreamEnded, err)
		}
		return nil, provider.TransportError(s.ctx, s.name, err)
	}

	var e event
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		s.done = true
		return nil, &provider.Error{Provider: s.name, Class: provider.Unavailable, Message: "malformed stream event", Err: err}
	}

	chunk := keepalive
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.input = e.Message.Usage.prompt()
			s.output = e.Message.Usage.OutputTokens
			chunk.Model = e.Message.Model
		}
	case "content_block_start":
		if e.ContentBlock != nil && isThinking(e.ContentBlock.Type) {
			chunk.Reasoning = true
		}
	case "content_block_delta":
		if e.Delta != nil {
			switch e.Delta.Type {
			case "text_delta":
				chunk.Delta = e.Delta.Text
			case "thinking_delta", "signature_delta":
				chunk.Reasoning = true
			}
		}
	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			s.stop = e.Delta.StopReason
		}
		if e.Usage != nil {
			s.output = e.Usage.OutputTokens
			if p := e.Usage.prompt(); p > 0 {
				s.input = p
			}
		}
	case "message_stop":
		s.done = true
		return &provider.Chunk{
			FinishReason: finishReason(s.stop),
			Usage:        &provider.Usage{PromptTokens: s.input, CompletionTokens: s.output},
		}, nil
	case "error":
		s.done = true
		return nil, s.eventError(e)
	}
	return &chunk, nil
}

// eventError turns an error event into a classified error. Anthropic says
// errors in a stream correspond to the HTTP errors, such as overloaded_error
// for a 529, so the error type decides the class.
func (s *stream) eventError(e event) error {
	typ, msg := "error", "error event in stream"
	if e.Error != nil {
		typ, msg = e.Error.Type, e.Error.Message
	}
	class := provider.Unavailable
	if typ == "rate_limit_error" {
		class = provider.RateLimited
	}
	return &provider.Error{Provider: s.name, Class: class, Message: fmt.Sprintf("%s: %s", typ, msg)}
}

func isThinking(blockType string) bool {
	return blockType == "thinking" || blockType == "redacted_thinking"
}

// Close releases the response body. It is safe to call more than once.
func (s *stream) Close() error {
	s.done = true
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}
