package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

func TestRequestIDs(t *testing.T) {
	var seen string
	h := withRequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	ids := map[string]bool{}
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		id := rec.Header().Get(HeaderRequestID)
		if !strings.HasPrefix(id, "req_") || len(id) != len("req_")+26 {
			t.Errorf("request ID %q, want req_ and 26 random characters", id)
		}
		if seen != id {
			t.Errorf("context has %q, header has %q", seen, id)
		}
		ids[id] = true
	}
	if len(ids) != 3 {
		t.Errorf("got %d distinct IDs for 3 requests", len(ids))
	}
}

func TestRecoverPanics(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := withRequestID(recoverPanics(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if e := decodeError(t, rec); deref(e.Code) != api.CodeInternal {
		t.Errorf("code = %q, want %q", deref(e.Code), api.CodeInternal)
	}
	for _, want := range []string{`"msg":"panic"`, `"panic":"boom"`, rec.Header().Get(HeaderRequestID), "middleware_test.go"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("panic log lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestRecoverPanicsLetsAbortThrough(t *testing.T) {
	h := recoverPanics(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v == nil || !errors.Is(v.(error), http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler to pass through", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
}

func TestAccessLog(t *testing.T) {
	var logs bytes.Buffer
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 3, Seed: 1}), nil)
	s.log = slog.New(slog.NewJSONHandler(&logs, nil))
	s.http.Handler = s.routes() // rebuild the middleware with the capturing logger

	const secret = "TOP-SECRET-PROMPT"
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions",
		`{"model": "mock-fast", "messages": [{"role": "user", "content": "`+secret+`"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	do(t.Context(), s, http.MethodGet, "/healthz", "")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1 (health checks log at debug):\n%s", len(lines), logs.String())
	}
	type logEntry struct {
		Msg       string `json:"msg"`
		RequestID string `json:"request_id"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Status    int    `json:"status"`
	}
	var got logEntry
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	want := logEntry{"request", rec.Header().Get(HeaderRequestID), "POST", "/v1/chat/completions", 200}
	if got != want {
		t.Errorf("log entry = %+v, want %+v", got, want)
	}

	// Prompts and completions never reach the logs.
	var reply api.ChatCompletion
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	for _, text := range []string{secret, reply.Choices[0].Message.Content} {
		if strings.Contains(logs.String(), text) {
			t.Errorf("logs contain request or response text %q", text)
		}
	}
}
