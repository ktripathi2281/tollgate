package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// upstream is a fake Messages API. It records the last request and answers
// with the given status, headers and body.
type upstream struct {
	srv     *httptest.Server
	request *http.Request
	body    map[string]any
}

func newUpstream(t *testing.T, status int, header http.Header, body []byte) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.request = r
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &u.body)
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func adapter(t *testing.T, baseURL string) *Provider {
	t.Helper()
	client := provider.NewHTTPClient(5 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	return New(Config{Name: "anthropic", BaseURL: baseURL, APIKey: "sk-ant-test", Client: client})
}

func request() *provider.ChatRequest {
	temp := 0.5
	return &provider.ChatRequest{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Parts: []string{"You are terse."}},
			{Role: provider.RoleUser, Parts: []string{"Hello", ""}},
			{Role: provider.RoleAssistant, Parts: []string{"Hi."}},
			{Role: provider.RoleDeveloper, Parts: []string{"Answer in English."}},
			{Role: provider.RoleUser, Parts: []string{"How are you?"}},
		},
		MaxTokens:   64,
		Temperature: &temp,
		Stop:        []string{"END"},
	}
}

func TestRequestOnTheWire(t *testing.T) {
	u := newUpstream(t, 200, nil, fixture(t, "message.json"))
	if _, err := adapter(t, u.srv.URL).Chat(t.Context(), request()); err != nil {
		t.Fatal(err)
	}

	r := u.request
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		t.Errorf("request %s %s, want POST /v1/messages", r.Method, r.URL.Path)
	}
	for header, want := range map[string]string{
		"X-Api-Key":         "sk-ant-test",
		"Anthropic-Version": APIVersion,
		"Content-Type":      "application/json",
	} {
		if got := r.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if r.Header.Get("Authorization") != "" {
		t.Error("the key must go in x-api-key, not Authorization")
	}

	block := func(text string) any { return map[string]any{"type": "text", "text": text} }
	want := map[string]any{
		"model":      "claude-x",
		"max_tokens": 64.0,
		// System and developer messages, wherever they were, move to the
		// top-level system prompt in order.
		"system": []any{block("You are terse."), block("Answer in English.")},
		"messages": []any{
			// The empty part is dropped: Anthropic rejects empty text blocks.
			map[string]any{"role": "user", "content": []any{block("Hello")}},
			map[string]any{"role": "assistant", "content": []any{block("Hi.")}},
			map[string]any{"role": "user", "content": []any{block("How are you?")}},
		},
		"temperature":    0.5,
		"stop_sequences": []any{"END"},
	}
	if !reflect.DeepEqual(u.body, want) {
		t.Errorf("body =\n%v\nwant\n%v", u.body, want)
	}
}

func TestChatResponses(t *testing.T) {
	tests := []struct {
		fixture string
		want    provider.ChatResponse
	}{
		// Thinking is left out; text blocks are joined; cached input
		// tokens count as input: 10 + 4 + 6.
		{"message.json", provider.ChatResponse{
			Model: "claude-x-2026", Content: "Hello! How can I help?", FinishReason: provider.FinishStop,
			Usage: &provider.Usage{PromptTokens: 20, CompletionTokens: 25},
		}},
		{"message_max_tokens.json", provider.ChatResponse{
			Model: "claude-x-2026", Content: "One two", FinishReason: provider.FinishLength,
			Usage: &provider.Usage{PromptTokens: 8, CompletionTokens: 2},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			u := newUpstream(t, 200, nil, fixture(t, tt.fixture))
			got, err := adapter(t, u.srv.URL).Chat(t.Context(), request())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestFinishReasons(t *testing.T) {
	for reason, want := range map[string]provider.FinishReason{
		"end_turn":                      provider.FinishStop,
		"stop_sequence":                 provider.FinishStop,
		"max_tokens":                    provider.FinishLength,
		"model_context_window_exceeded": provider.FinishLength,
		"refusal":                       provider.FinishContentFilter,
		"tool_use":                      provider.FinishStop,
		"pause_turn":                    provider.FinishStop,
		"something_new":                 provider.FinishStop,
	} {
		if got := finishReason(reason); got != want {
			t.Errorf("finishReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		header         http.Header
		fixture        string
		wantClass      provider.Class
		wantMessage    string
		wantRetryAfter time.Duration
	}{
		{"bad request", 400, nil, "error_400.json", provider.BadRequest, "must be non-empty", 0},
		{"bad key", 401, nil, "error_401.json", provider.AuthOrConfig, "invalid x-api-key", 0},
		{"billing", 402, nil, "error_402.json", provider.AuthOrConfig, "credit balance", 0},
		{"unknown model", 404, nil, "error_404.json", provider.AuthOrConfig, "claude-typo", 0},
		{"rate limit", 429, http.Header{"Retry-After": {"5"}}, "error_429.json", provider.RateLimited, "rate limit", 5 * time.Second},
		{"overloaded", 529, nil, "error_529.json", provider.Unavailable, "Overloaded", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, tt.status, tt.header, fixture(t, tt.fixture))
			_, err := adapter(t, u.srv.URL).Chat(t.Context(), request())
			perr, ok := errors.AsType[*provider.Error](err)
			if !ok {
				t.Fatalf("got %v, want a *provider.Error", err)
			}
			if perr.Class != tt.wantClass || perr.Status != tt.status || perr.RetryAfter != tt.wantRetryAfter {
				t.Errorf("class %v, status %d, retry after %v; want %v, %d, %v",
					perr.Class, perr.Status, perr.RetryAfter, tt.wantClass, tt.status, tt.wantRetryAfter)
			}
			if !strings.Contains(perr.Message, tt.wantMessage) {
				t.Errorf("message %q, want it to contain %q", perr.Message, tt.wantMessage)
			}
		})
	}
}

func TestChatCancelled(t *testing.T) {
	u := newUpstream(t, 200, nil, fixture(t, "message.json"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := adapter(t, u.srv.URL).Chat(ctx, request()); provider.ClassOf(err) != provider.Cancelled {
		t.Errorf("got %v, want Cancelled", err)
	}
}

func TestValidate(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	user := func(parts ...string) provider.Message { return provider.Message{Role: provider.RoleUser, Parts: parts} }
	system := provider.Message{Role: provider.RoleSystem, Parts: []string{"s"}}
	tests := []struct {
		name      string
		req       provider.ChatRequest
		wantParam string // "" means valid
	}{
		{"plain", provider.ChatRequest{Messages: []provider.Message{user("hi")}}, ""},
		{"temperature at the top of the range", provider.ChatRequest{Messages: []provider.Message{user("hi")}, Temperature: f(1)}, ""},
		{"top_p alone", provider.ChatRequest{Messages: []provider.Message{user("hi")}, TopP: f(0.5)}, ""},
		{"temperature above 1", provider.ChatRequest{Messages: []provider.Message{user("hi")}, Temperature: f(1.5)}, "temperature"},
		{"temperature and top_p", provider.ChatRequest{Messages: []provider.Message{user("hi")}, Temperature: f(0.5), TopP: f(0.5)}, "top_p"},
		{"empty user message", provider.ChatRequest{Messages: []provider.Message{user("")}}, "messages"},
		{"only a system message", provider.ChatRequest{Messages: []provider.Message{system}}, "messages"},
		{"an empty part beside text is fine", provider.ChatRequest{Messages: []provider.Message{system, user("", "hi")}}, ""},
	}
	p := New(Config{Name: "anthropic"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Validate(&tt.req)
			if tt.wantParam == "" {
				if err != nil {
					t.Errorf("Validate = %v, want nil", err)
				}
				return
			}
			perr, ok := errors.AsType[*provider.ParamError](err)
			if !ok || perr.Param != tt.wantParam {
				t.Errorf("Validate = %v, want a ParamError for %q", err, tt.wantParam)
			}
		})
	}
}
