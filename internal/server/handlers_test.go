package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

const helloBody = `{"model": "mock-fast", "messages": [{"role": "user", "content": "Hello there!"}]}`

// decodeError checks that rec holds an OpenAI error envelope and returns it.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) api.EnvelopeError {
	t.Helper()
	var env api.Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an error envelope: %v", err)
	}
	if env.Error.Message == "" || env.Error.Type == "" {
		t.Errorf("error envelope is missing its message or type: %+v", env.Error)
	}
	return env.Error
}

func TestChatCompletion(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 3, Seed: 1}), nil)

	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", helloBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":  "application/json",
		HeaderProvider:  "mock",
		HeaderAttempts:  "1",
		HeaderRequestID: "req_",
	} {
		if got := rec.Header().Get(header); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// OpenAI sends logprobs and refusal as explicit nulls; SDKs expect them.
	raw := rec.Body.String()
	for _, want := range []string{`"logprobs":null`, `"refusal":null`} {
		if !strings.Contains(raw, want) {
			t.Errorf("body lacks %s: %s", want, raw)
		}
	}

	var got api.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ID, "chatcmpl-") || got.Object != "chat.completion" ||
		got.Created != fixedTime.Unix() || got.Model != "mock-1" {
		t.Errorf("id, object, created, model = %q, %q, %d, %q", got.ID, got.Object, got.Created, got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Index != 0 || c.Message.Role != "assistant" || c.FinishReason != "stop" || len(strings.Fields(c.Message.Content)) != 3 {
		t.Errorf("choice = %+v, want a 3-word assistant reply that finished with stop", c)
	}
	// "Hello there!" is 12 characters, so about 3 prompt tokens.
	if want := (api.Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6}); got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
}

func TestChatCompletionTokenLimits(t *testing.T) {
	tests := []struct {
		name       string
		limit      string // extra JSON field, or ""
		wantTokens int
		wantFinish string
	}{
		{"alias default when the client sets none", ``, 5, "length"},
		{"client max_tokens", `"max_tokens": 2,`, 2, "length"},
		{"client max_completion_tokens at the ceiling", `"max_completion_tokens": 50,`, 10, "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 10}), nil)
			body := `{` + tt.limit + `"model": "mock-fast", "messages": [{"role": "user", "content": "hi"}]}`
			rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", body)

			var got api.ChatCompletion
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if got.Usage.CompletionTokens != tt.wantTokens || got.Choices[0].FinishReason != tt.wantFinish {
				t.Errorf("completion tokens, finish = %d, %q; want %d, %q",
					got.Usage.CompletionTokens, got.Choices[0].FinishReason, tt.wantTokens, tt.wantFinish)
			}
		})
	}
}

func TestChatCompletionErrors(t *testing.T) {
	tests := []struct {
		name           string
		mock           mock.Config
		body           string
		wantStatus     int
		wantCode       string
		wantRetryAfter string
	}{
		{name: "invalid JSON", body: `{`, wantStatus: 400, wantCode: api.CodeInvalidJSON},
		{name: "unsupported parameter", body: `{"model": "mock-fast", "messages": [], "tools": [{}]}`, wantStatus: 400, wantCode: api.CodeUnsupportedValue},
		{name: "unknown model", body: `{"model": "gpt-9", "messages": [{"role": "user", "content": "hi"}]}`, wantStatus: 404, wantCode: api.CodeModelNotFound},
		{name: "streaming not yet supported", body: `{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`, wantStatus: 400, wantCode: api.CodeUnsupportedValue},
		{name: "token limit above the ceiling", body: `{"model": "mock-fast", "max_tokens": 51, "messages": [{"role": "user", "content": "hi"}]}`, wantStatus: 400, wantCode: api.CodeInvalidValue},
		{name: "upstream bad request", mock: mock.Config{Status: 400}, body: helloBody, wantStatus: 400},
		{name: "upstream auth failure", mock: mock.Config{Status: 401}, body: helloBody, wantStatus: 502, wantCode: api.CodeUpstreamError},
		{name: "upstream rate limit", mock: mock.Config{Status: 429}, body: helloBody, wantStatus: 503, wantCode: api.CodeUpstreamRateLimited, wantRetryAfter: "1"},
		{name: "upstream server error", mock: mock.Config{Status: 500}, body: helloBody, wantStatus: 502, wantCode: api.CodeUpstreamError},
		{name: "upstream overloaded", mock: mock.Config{Status: 529}, body: helloBody, wantStatus: 502, wantCode: api.CodeUpstreamError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.mock.OutputTokens = 3
			s := testServer(t, mock.New("mock", tt.mock), nil)
			rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", tt.body)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			e := decodeError(t, rec)
			if got := deref(e.Code); got != tt.wantCode {
				t.Errorf("code = %q, want %q (message: %s)", got, tt.wantCode, e.Message)
			}
			// Upstream detail stays in the logs.
			if tt.mock.Status != 0 && tt.mock.Status != 400 && strings.Contains(e.Message, "mock failure") {
				t.Errorf("message leaks upstream detail: %q", e.Message)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestChatCompletionBodyTooLarge(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), func(c *config.Server) { c.MaxBodyBytes = 100 })
	body := `{"model": "mock-fast", "messages": [{"role": "user", "content": "` + strings.Repeat("x", 200) + `"}]}`

	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if e := decodeError(t, rec); deref(e.Code) != api.CodeRequestTooLarge {
		t.Errorf("code = %q, want %q", deref(e.Code), api.CodeRequestTooLarge)
	}
}

func TestChatCompletionUpstreamTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1, Hang: true}), nil)
		// The request context's deadline stands in for the gateway's own
		// timeouts, which arrive in M2. The clock is fake inside synctest.
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		rec := do(ctx, s, http.MethodPost, "/v1/chat/completions", helloBody)
		if rec.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", rec.Code)
		}
		if e := decodeError(t, rec); deref(e.Code) != api.CodeUpstreamTimeout {
			t.Errorf("code = %q, want %q", deref(e.Code), api.CodeUpstreamTimeout)
		}
	})
}

func TestChatCompletionClientGoneWritesNothing(t *testing.T) {
	g := newGate()
	s := testServer(t, g, nil)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan *strings.Builder)
	go func() {
		rec := do(ctx, s, http.MethodPost, "/v1/chat/completions", helloBody)
		var b strings.Builder
		b.Write(rec.Body.Bytes())
		done <- &b
	}()
	<-g.arrived
	cancel()
	if body := <-done; body.Len() != 0 {
		t.Errorf("wrote %q to a client that had gone, want nothing", body)
	}
}

func TestModels(t *testing.T) {
	s := testServer(t, newGate(), nil)
	rec := do(t.Context(), s, http.MethodGet, "/v1/models", "")

	var got api.ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	want := api.ModelList{Object: "list", Data: []api.Model{
		{ID: "another", Object: "model", Created: fixedTime.Unix(), OwnedBy: "system"},
		{ID: "mock-fast", Object: "model", Created: fixedTime.Unix(), OwnedBy: "system"},
	}}
	if len(got.Data) != 2 || got.Object != want.Object || got.Data[0] != want.Data[0] || got.Data[1] != want.Data[1] {
		t.Errorf("models = %+v, want %+v", got, want)
	}
}

func TestUnknownAPIPaths(t *testing.T) {
	s := testServer(t, newGate(), nil)
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/v1/embeddings"},
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/models"},
	} {
		rec := do(t.Context(), s, tt.method, tt.path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", tt.method, tt.path, rec.Code)
		}
		if e := decodeError(t, rec); !strings.Contains(e.Message, tt.method+" "+tt.path) {
			t.Errorf("%s %s: message = %q, want it to name the request", tt.method, tt.path, e.Message)
		}
	}
}

// Acceptance (M1): the in-flight cap sheds with 503 under concurrency.
func TestInflightCapShedsExcessRequests(t *testing.T) {
	const capacity, total = 10, 50
	g := newGate()
	s := testServer(t, g, func(c *config.Server) { c.MaxInflight = capacity })

	codes := make(chan int, total)
	for range total {
		go func() {
			codes <- do(t.Context(), s, http.MethodPost, "/v1/chat/completions", helloBody).Code
		}()
	}

	// The gate holds every admitted request, so the cap admits exactly
	// `capacity` of them and sheds the rest at once.
	for range capacity {
		<-g.arrived
	}
	for range total - capacity {
		if code := <-codes; code != http.StatusServiceUnavailable {
			t.Errorf("shed request: status = %d, want 503", code)
		}
	}
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", helloBody)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("while full: status %d, Retry-After %q; want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if e := decodeError(t, rec); deref(e.Code) != api.CodeOverloaded {
		t.Errorf("code = %q, want %q", deref(e.Code), api.CodeOverloaded)
	}
	// Health checks are not behind the cap.
	if rec := do(t.Context(), s, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz while full: status = %d, want 200", rec.Code)
	}

	// Releasing the gate finishes the admitted requests and frees their
	// slots for new ones.
	close(g.release)
	for range capacity {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("admitted request: status = %d, want 200", code)
		}
	}
	if rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", helloBody); rec.Code != http.StatusOK {
		t.Errorf("after release: status = %d, want 200", rec.Code)
	}
}
