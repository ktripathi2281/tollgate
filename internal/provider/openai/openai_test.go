package openai

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

// upstream is a fake OpenAI server. It records the last request and answers
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

func adapter(t *testing.T, baseURL, tokenParam string) *Provider {
	t.Helper()
	client := provider.NewHTTPClient(5 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	return New(Config{Name: "openai", BaseURL: baseURL + "/v1", APIKey: "sk-test", TokenParam: tokenParam, Client: client})
}

func request() *provider.ChatRequest {
	temp, topP := 0.0, 0.9
	return &provider.ChatRequest{
		Model: "gpt-x",
		Messages: []provider.Message{
			{Role: provider.RoleDeveloper, Parts: []string{"Be brief."}},
			{Role: provider.RoleUser, Parts: []string{"Hello", " there"}},
		},
		MaxTokens:   64,
		Temperature: &temp,
		TopP:        &topP,
		Stop:        []string{"END"},
	}
}

func TestChatRequestOnTheWire(t *testing.T) {
	u := newUpstream(t, 200, nil, fixture(t, "chat.json"))
	if _, err := adapter(t, u.srv.URL, "").Chat(t.Context(), request()); err != nil {
		t.Fatal(err)
	}

	r := u.request
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		t.Errorf("request %s %s, want POST /v1/chat/completions", r.Method, r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	want := map[string]any{
		"model": "gpt-x",
		"messages": []any{
			// A one-part message is a plain string; parts stay parts.
			map[string]any{"role": "developer", "content": "Be brief."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Hello"},
				map[string]any{"type": "text", "text": " there"},
			}},
		},
		"max_completion_tokens": 64.0,
		"temperature":           0.0, // zero is sent, not omitted
		"top_p":                 0.9,
		"stop":                  []any{"END"},
	}
	if !reflect.DeepEqual(u.body, want) {
		t.Errorf("body =\n%v\nwant\n%v", u.body, want)
	}
}

func TestChatTokenParam(t *testing.T) {
	u := newUpstream(t, 200, nil, fixture(t, "chat.json"))
	if _, err := adapter(t, u.srv.URL, TokenParamMaxTokens).Chat(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.body["max_completion_tokens"]; ok || u.body["max_tokens"] != 64.0 {
		t.Errorf("body = %v, want max_tokens: 64 and no max_completion_tokens", u.body)
	}
}

func TestChatResponses(t *testing.T) {
	tests := []struct {
		fixture string
		want    provider.ChatResponse
	}{
		{"chat.json", provider.ChatResponse{
			Model: "gpt-x-2026-09-01", Content: "Hello! How can I help?", FinishReason: provider.FinishStop,
			Usage: &provider.Usage{PromptTokens: 12, CompletionTokens: 7},
		}},
		// A compatible server that reports no usage, cut short by the limit.
		{"chat_length_no_usage.json", provider.ChatResponse{
			Model: "llama-local", Content: "One two three", FinishReason: provider.FinishLength,
		}},
		{"chat_refusal.json", provider.ChatResponse{
			Model: "gpt-x-2026-09-01", Content: "I can't help with that.", FinishReason: provider.FinishContentFilter,
			Usage: &provider.Usage{PromptTokens: 20, CompletionTokens: 6},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			u := newUpstream(t, 200, nil, fixture(t, tt.fixture))
			got, err := adapter(t, u.srv.URL, "").Chat(t.Context(), request())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		header         http.Header
		body           []byte
		wantClass      provider.Class
		wantMessage    string
		wantRetryAfter time.Duration
	}{
		{"bad request", 400, nil, fixture(t, "error_400.json"), provider.BadRequest, "string too long", 0},
		{"bad key", 401, nil, fixture(t, "error_401.json"), provider.AuthOrConfig, "Incorrect API key", 0},
		{"unknown model", 404, nil, fixture(t, "error_404.json"), provider.AuthOrConfig, "does not exist", 0},
		{"rate limit", 429, http.Header{"Retry-After": {"3"}}, fixture(t, "error_429_rate.json"), provider.RateLimited, "Rate limit reached", 3 * time.Second},
		{"quota is a config error", 429, nil, fixture(t, "error_429_quota.json"), provider.AuthOrConfig, "exceeded your current quota", 0},
		{"overloaded", 503, http.Header{"Retry-After": {"1"}}, fixture(t, "error_503.json"), provider.Unavailable, "overloaded", time.Second},
		{"not JSON", 502, nil, []byte("<html>Bad Gateway</html>"), provider.Unavailable, "Bad Gateway", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, tt.status, tt.header, tt.body)
			_, err := adapter(t, u.srv.URL, "").Chat(t.Context(), request())
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

func TestChatBadSuccessResponse(t *testing.T) {
	for _, body := range []string{`not json`, `{"choices": []}`} {
		u := newUpstream(t, 200, nil, []byte(body))
		_, err := adapter(t, u.srv.URL, "").Chat(t.Context(), request())
		if provider.ClassOf(err) != provider.Unavailable {
			t.Errorf("200 with %q: got %v, want Unavailable", body, err)
		}
	}
}

func TestTransportFailures(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close() // nothing listens there now
		_, err := adapter(t, url, "").Chat(t.Context(), request())
		if provider.ClassOf(err) != provider.Unavailable {
			t.Errorf("got %v, want Unavailable", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		u := newUpstream(t, 200, nil, fixture(t, "chat.json"))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := adapter(t, u.srv.URL, "").Chat(ctx, request())
		if provider.ClassOf(err) != provider.Cancelled {
			t.Errorf("got %v, want Cancelled", err)
		}
	})
}
