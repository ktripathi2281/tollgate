package mock

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/api"
)

// readEvents reads server-sent events until the body ends and returns the
// data of each one.
func readEvents(t *testing.T, body io.Reader) []string {
	t.Helper()
	var events []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			events = append(events, data)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading events: %v", err)
	}
	return events
}

// streamFrom sends body to a mock server built from cfg and returns the
// response headers and the events that arrived.
func streamFrom(t *testing.T, cfg Config, body string) (http.Header, []string) {
	t.Helper()
	srv := httptest.NewServer(Handler(New("mock", cfg)))
	t.Cleanup(srv.Close)
	resp := post(t, t.Context(), srv, body)
	defer resp.Body.Close()
	return resp.Header, readEvents(t, resp.Body)
}

func TestHandlerStream(t *testing.T) {
	header, events := streamFrom(t, Config{OutputTokens: 3, Seed: 1}, `{
		"model": "mock-1",
		"messages": [{"role": "user", "content": "Hello there"}],
		"stream": true,
		"stream_options": {"include_usage": true}
	}`)

	if ct := header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// role, 3 words, finish, usage, [DONE]
	if len(events) != 7 || events[6] != "[DONE]" {
		t.Fatalf("got %d events, want 7 ending in [DONE]:\n%s", len(events), strings.Join(events, "\n"))
	}
	chunks := make([]api.ChatCompletionChunk, 6)
	for i := range chunks {
		if err := json.Unmarshal([]byte(events[i]), &chunks[i]); err != nil {
			t.Fatalf("event %d is not a chunk: %v", i, err)
		}
		if chunks[i].Object != "chat.completion.chunk" || chunks[i].ID != chunks[0].ID {
			t.Errorf("event %d: object %q, id %q; want chat.completion.chunk and the stream's id", i, chunks[i].Object, chunks[i].ID)
		}
	}
	if d := chunks[0].Choices[0].Delta; d.Role != "assistant" {
		t.Errorf("first chunk delta = %+v, want the assistant role", d)
	}
	var text strings.Builder
	for _, c := range chunks[1:4] {
		text.WriteString(*c.Choices[0].Delta.Content)
	}
	if len(strings.Fields(text.String())) != 3 {
		t.Errorf("streamed text %q, want 3 words", text.String())
	}
	if f := chunks[4].Choices[0].FinishReason; f == nil || *f != "stop" {
		t.Errorf("finish chunk = %+v, want finish_reason stop", chunks[4])
	}
	if u := chunks[5].Usage; len(chunks[5].Choices) != 0 || u == nil || *u != (api.Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6}) {
		t.Errorf("usage chunk = %+v, want no choices and 3+3 tokens", chunks[5])
	}
}

func TestHandlerStreamWithoutUsage(t *testing.T) {
	_, events := streamFrom(t, Config{OutputTokens: 2}, `{"model": "m", "messages": [], "stream": true}`)
	// role, 2 words, finish, [DONE]: no usage chunk unless the client asks.
	if len(events) != 5 || strings.Contains(strings.Join(events, ""), `"usage"`) {
		t.Errorf("got events %q, want 5 and no usage", events)
	}
}

func TestHandlerStreamErrorEvent(t *testing.T) {
	_, events := streamFrom(t, Config{OutputTokens: 5, FailAtChunk: 2}, `{"model": "m", "messages": [], "stream": true}`)
	// role, 1 word, then the error in place of the second word, and no [DONE].
	if len(events) != 3 {
		t.Fatalf("got events %q, want 3", events)
	}
	var env api.Envelope
	if err := json.Unmarshal([]byte(events[2]), &env); err != nil || env.Error.Type != api.TypeServer {
		t.Errorf("last event %q, want an error object", events[2])
	}
}
