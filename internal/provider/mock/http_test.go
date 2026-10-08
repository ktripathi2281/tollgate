package mock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
)

// post sends a chat completion request to the mock server.
func post(t *testing.T, ctx context.Context, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestHandlerChatCompletion(t *testing.T) {
	srv := httptest.NewServer(Handler(New("mock", Config{OutputTokens: 4, Seed: 1})))
	defer srv.Close()

	resp := post(t, t.Context(), srv, `{
		"model": "mock-1",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello there"}]}],
		"max_completion_tokens": 3,
		"seed": 42
	}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got api.ChatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || got.Model != "mock-1" || len(got.Choices) != 1 {
		t.Fatalf("unexpected response: %+v", got)
	}
	if c := got.Choices[0]; c.FinishReason != "length" || len(strings.Fields(c.Message.Content)) != 3 {
		t.Errorf("choice = %+v, want 3 words cut short by the limit", c)
	}
	if want := (api.Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6}); got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
}

func TestHandlerErrors(t *testing.T) {
	tests := []struct {
		name           string
		cfg            Config
		body           string
		wantStatus     int
		wantType       string
		wantRetryAfter string
	}{
		{"invalid JSON", Config{OutputTokens: 1}, `{`, 400, api.TypeInvalidRequest, ""},
		{"forced status on a stream", Config{OutputTokens: 1, Status: 503}, `{"model": "m", "messages": [], "stream": true}`, 503, api.TypeServer, ""},
		{"forced 429", Config{OutputTokens: 1, Status: 429}, `{"model": "m", "messages": []}`, 429, api.TypeInvalidRequest, "1"},
		{"forced 503", Config{OutputTokens: 1, Status: 503}, `{"model": "m", "messages": []}`, 503, api.TypeServer, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(Handler(New("mock", tt.cfg)))
			defer srv.Close()

			resp := post(t, t.Context(), srv, tt.body)
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Header.Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			var env api.Envelope
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("body is not an error envelope: %v", err)
			}
			if env.Error.Type != tt.wantType || env.Error.Message == "" {
				t.Errorf("error = %+v, want type %q and a message", env.Error, tt.wantType)
			}
		})
	}
}

// The mock sees a client disconnect through the request context.
func TestHandlerSeesClientCancel(t *testing.T) {
	p := New("mock", Config{OutputTokens: 1, Hang: true})
	arrived := make(chan struct{})
	cancelled := make(chan struct{})
	p.OnCall = func() { close(arrived) }
	p.OnCancel = func() { close(cancelled) }

	srv := httptest.NewServer(Handler(p))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model": "m", "messages": []}`))
		if resp, err := srv.Client().Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	<-arrived
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the mock never saw the cancellation")
	}
	<-done
}

func TestHandlerModels(t *testing.T) {
	srv := httptest.NewServer(Handler(New("mock", Config{OutputTokens: 1})))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list api.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "mock-1" {
		t.Errorf("models = %+v, want one model, mock-1", list)
	}
}
