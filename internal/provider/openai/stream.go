package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/sse"
)

// stream reads an OpenAI chat.completion.chunk stream. Its context is the
// one the call was made with; cancelling it aborts the body read.
type stream struct {
	ctx    context.Context
	name   string
	body   io.ReadCloser
	events *sse.Reader
	done   bool
	closed bool
}

func newStream(ctx context.Context, name string, body io.ReadCloser) *stream {
	return &stream{ctx: ctx, name: name, body: body, events: sse.NewReader(body)}
}

// chunk is the part of a chat.completion.chunk the adapter reads. A chunk
// can also be an error object, sent in place of a chunk mid-stream.
type chunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Next returns the next chunk. The first chunk, which only says the reply is
// from the assistant, comes back as a keepalive carrying the model. The
// usage chunk, with no choices, comes back carrying only usage.
func (s *stream) Next() (*provider.Chunk, error) {
	if s.done {
		return nil, io.EOF
	}
	ev, err := s.events.Next()
	if err != nil {
		s.done = true
		switch {
		case errors.Is(err, io.EOF):
			// The body ended cleanly but without [DONE]. The error must not
			// wrap io.EOF: callers take io.EOF to mean a complete stream,
			// and this one was cut short.
			err = errStreamEnded
		case errors.Is(err, io.ErrUnexpectedEOF):
			err = fmt.Errorf("%w: %w", errStreamEnded, err)
		}
		return nil, provider.TransportError(s.ctx, s.name, err)
	}
	if ev.Data == "[DONE]" {
		s.done = true
		return nil, io.EOF
	}

	var c chunk
	if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
		s.done = true
		return nil, &provider.Error{Provider: s.name, Class: provider.Unavailable, Message: "malformed stream chunk", Err: err}
	}
	if c.Error != nil {
		// An error object mid-stream: the upstream failed after the
		// response started, which is Unavailable whatever its type.
		s.done = true
		return nil, &provider.Error{Provider: s.name, Class: provider.Unavailable, Message: c.Error.Message}
	}

	out := &provider.Chunk{Model: c.Model, Usage: c.Usage.canonical()}
	if len(c.Choices) > 0 {
		choice := c.Choices[0]
		out.Delta = deref(choice.Delta.Content) + deref(choice.Delta.Refusal)
		if choice.FinishReason != nil {
			out.FinishReason = finishReason(*choice.FinishReason)
		}
	}
	return out, nil
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
