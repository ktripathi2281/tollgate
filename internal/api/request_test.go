package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// body builds a request body from a minimal valid request with some
// parameters replaced. Each value is raw JSON; the value "-" removes the
// parameter instead.
func body(t *testing.T, params map[string]string) []byte {
	t.Helper()
	fields := map[string]json.RawMessage{
		"model":    json.RawMessage(`"mock-fast"`),
		"messages": json.RawMessage(`[{"role": "user", "content": "hi"}]`),
	}
	for name, raw := range params {
		if raw == "-" {
			delete(fields, name)
			continue
		}
		fields[name] = json.RawMessage(raw)
	}
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("building body: %v", err)
	}
	return b
}

// minimalRequest is what the minimal body parses to.
func minimalRequest() ChatCompletionRequest {
	return ChatCompletionRequest{
		Model:    "mock-fast",
		Messages: []provider.Message{{Role: provider.RoleUser, Parts: []string{"hi"}}},
	}
}

func ptr[T any](v T) *T { return &v }

// mustParse parses a body that should be valid.
func mustParse(t *testing.T, b []byte) ChatCompletionRequest {
	t.Helper()
	req, err := ParseChatCompletionRequest(b)
	if err != nil {
		t.Fatalf("ParseChatCompletionRequest(%s): unexpected error: %v", b, err)
	}
	return *req
}

// wantError checks that err is an *Error with status 400 and the given code
// and param.
func wantError(t *testing.T, err error, code, param string) {
	t.Helper()
	apiErr, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("got error %v (%T), want an *api.Error", err, err)
	}
	if apiErr.Status != 400 || apiErr.Type != TypeInvalidRequest {
		t.Errorf("status, type = %d, %q; want 400, %q", apiErr.Status, apiErr.Type, TypeInvalidRequest)
	}
	if apiErr.Code != code || apiErr.Param != param {
		t.Errorf("code, param = %q, %q; want %q, %q (message: %s)", apiErr.Code, apiErr.Param, code, param, apiErr.Message)
	}
}

// openAIParams is every top-level request parameter in OpenAI's chat
// completions API reference, checked October 2026.
var openAIParams = []string{
	"messages", "model", "audio", "frequency_penalty", "function_call", "functions",
	"logit_bias", "logprobs", "max_completion_tokens", "max_tokens", "metadata",
	"modalities", "moderation", "n", "parallel_tool_calls", "prediction",
	"presence_penalty", "prompt_cache_key", "prompt_cache_options",
	"prompt_cache_retention", "reasoning_effort", "response_format",
	"safety_identifier", "seed", "service_tier", "stop", "store", "stream",
	"stream_options", "temperature", "tool_choice", "tools", "top_logprobs",
	"top_p", "user", "verbosity", "web_search_options",
}

// Every OpenAI parameter is in exactly one group, and the groups hold
// nothing else.
func TestParamGroupsCoverOpenAIAPI(t *testing.T) {
	for _, name := range openAIParams {
		n := 0
		if supportedParams[name] {
			n++
		}
		if ignoredParams[name] {
			n++
		}
		if _, ok := restrictedParams[name]; ok {
			n++
		}
		if n != 1 {
			t.Errorf("%s is in %d groups, want exactly 1", name, n)
		}
	}
	if total := len(supportedParams) + len(ignoredParams) + len(restrictedParams); total != len(openAIParams) {
		t.Errorf("the groups hold %d parameters, but OpenAI has %d", total, len(openAIParams))
	}
}

func TestParseMinimal(t *testing.T) {
	if got, want := mustParse(t, body(t, nil)), minimalRequest(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseSupportedParams(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
		want   func(*ChatCompletionRequest)
	}{
		{"stream true", map[string]string{"stream": `true`}, func(r *ChatCompletionRequest) { r.Stream = true }},
		{"stream false", map[string]string{"stream": `false`}, nil},
		{"stream null", map[string]string{"stream": `null`}, nil},
		{
			"include_usage", map[string]string{"stream": `true`, "stream_options": `{"include_usage": true}`},
			func(r *ChatCompletionRequest) { r.Stream, r.IncludeUsage = true, true },
		},
		{
			"include_obfuscation is ignored", map[string]string{"stream": `true`, "stream_options": `{"include_obfuscation": false}`},
			func(r *ChatCompletionRequest) { r.Stream = true },
		},
		{"stream_options null", map[string]string{"stream_options": `null`}, nil},
		{"max_tokens", map[string]string{"max_tokens": `100`}, func(r *ChatCompletionRequest) { r.MaxTokens = 100 }},
		{"max_completion_tokens", map[string]string{"max_completion_tokens": `200`}, func(r *ChatCompletionRequest) { r.MaxTokens = 200 }},
		{
			"max_tokens null with max_completion_tokens",
			map[string]string{"max_tokens": `null`, "max_completion_tokens": `5`},
			func(r *ChatCompletionRequest) { r.MaxTokens = 5 },
		},
		{"temperature 0", map[string]string{"temperature": `0`}, func(r *ChatCompletionRequest) { r.Temperature = ptr(0.0) }},
		{"temperature 2", map[string]string{"temperature": `2`}, func(r *ChatCompletionRequest) { r.Temperature = ptr(2.0) }},
		{"temperature 0.7", map[string]string{"temperature": `0.7`}, func(r *ChatCompletionRequest) { r.Temperature = ptr(0.7) }},
		{"temperature null", map[string]string{"temperature": `null`}, nil},
		{"top_p 0", map[string]string{"top_p": `0`}, func(r *ChatCompletionRequest) { r.TopP = ptr(0.0) }},
		{"top_p 1", map[string]string{"top_p": `1`}, func(r *ChatCompletionRequest) { r.TopP = ptr(1.0) }},
		{"stop null", map[string]string{"stop": `null`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := minimalRequest()
			if tt.want != nil {
				tt.want(&want)
			}
			if got := mustParse(t, body(t, tt.params)); !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// These cases go through parseStop, the M1 exercise.
func TestParseStopParam(t *testing.T) {
	got := mustParse(t, body(t, map[string]string{"stop": `["END", "STOP"]`}))
	if want := []string{"END", "STOP"}; !slices.Equal(got.Stop, want) {
		t.Errorf("Stop = %q, want %q", got.Stop, want)
	}

	_, err := ParseChatCompletionRequest(body(t, map[string]string{"stop": `["a", "b", "c", "d", "e"]`}))
	wantError(t, err, CodeInvalidValue, "stop")
}

func TestParseIgnoredParams(t *testing.T) {
	values := map[string]string{
		"user":                   `"user-123"`,
		"metadata":               `{"team": "search"}`,
		"store":                  `true`,
		"safety_identifier":      `"abc"`,
		"prompt_cache_key":       `"k"`,
		"prompt_cache_options":   `{"ttl": "1h"}`,
		"prompt_cache_retention": `"24h"`,
		"service_tier":           `"flex"`,
		"parallel_tool_calls":    `false`,
		"prediction":             `{"type": "content", "content": "x"}`,
	}
	if len(values) != len(ignoredParams) {
		t.Fatalf("test covers %d ignored parameters, want all %d", len(values), len(ignoredParams))
	}
	for name, raw := range values {
		t.Run(name, func(t *testing.T) {
			if got, want := mustParse(t, body(t, map[string]string{name: raw})), minimalRequest(); !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want the parameter dropped: %+v", got, want)
			}
		})
	}
}

func TestParseRestrictedParams(t *testing.T) {
	tests := []struct {
		name     string
		accepted []string // values that change nothing; null is always tried too
		rejected string   // a value that would change the output
	}{
		{"n", []string{`1`, `1.0`}, `2`},
		{"logprobs", []string{`false`}, `true`},
		{"top_logprobs", []string{`0`}, `5`},
		{"presence_penalty", []string{`0`, `0.0`, `-0`}, `0.5`},
		{"frequency_penalty", []string{`0`, `0.0`}, `-1`},
		{"logit_bias", []string{`{}`, `{ }`}, `{"50256": -100}`},
		{"tools", []string{`[]`}, `[{"type": "function", "function": {"name": "f"}}]`},
		{"functions", []string{`[]`}, `[{"name": "f"}]`},
		{"tool_choice", []string{`"none"`}, `"auto"`},
		{"function_call", []string{`"none"`}, `"auto"`},
		{"response_format", []string{`{"type": "text"}`, `{ "type":"text" }`}, `{"type": "json_object"}`},
		{"modalities", []string{`["text"]`}, `["text", "audio"]`},
		{"seed", nil, `42`},
		{"audio", nil, `{"voice": "alloy", "format": "mp3"}`},
		{"reasoning_effort", nil, `"high"`},
		{"verbosity", nil, `"low"`},
		{"web_search_options", nil, `{}`},
		{"moderation", nil, `{"input": true}`},
	}
	tested := map[string]bool{}
	for _, tt := range tests {
		tested[tt.name] = true
		t.Run(tt.name, func(t *testing.T) {
			for _, raw := range append([]string{`null`}, tt.accepted...) {
				if got, want := mustParse(t, body(t, map[string]string{tt.name: raw})), minimalRequest(); !reflect.DeepEqual(got, want) {
					t.Errorf("%s=%s: got %+v, want the parameter dropped", tt.name, raw, got)
				}
			}
			_, err := ParseChatCompletionRequest(body(t, map[string]string{tt.name: tt.rejected}))
			wantCode := CodeUnsupportedValue
			if len(tt.accepted) == 0 {
				wantCode = CodeUnsupportedParameter
			}
			wantError(t, err, wantCode, tt.name)
		})
	}
	for name := range restrictedParams {
		if !tested[name] {
			t.Errorf("restricted parameter %s has no test case", name)
		}
	}
}

func TestParseMessages(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		want     []provider.Message
	}{
		{
			name:     "every role",
			messages: `[{"role": "system", "content": "s"}, {"role": "developer", "content": "d"}, {"role": "user", "content": "u"}, {"role": "assistant", "content": "a"}]`,
			want: []provider.Message{
				{Role: provider.RoleSystem, Parts: []string{"s"}},
				{Role: provider.RoleDeveloper, Parts: []string{"d"}},
				{Role: provider.RoleUser, Parts: []string{"u"}},
				{Role: provider.RoleAssistant, Parts: []string{"a"}},
			},
		},
		{
			name:     "text parts keep their order",
			messages: `[{"role": "user", "content": [{"type": "text", "text": "one"}, {"type": "text", "text": "two"}]}]`,
			want:     []provider.Message{{Role: provider.RoleUser, Parts: []string{"one", "two"}}},
		},
		{
			name:     "empty string content",
			messages: `[{"role": "user", "content": ""}]`,
			want:     []provider.Message{{Role: provider.RoleUser, Parts: []string{""}}},
		},
		{
			name:     "no-op message fields",
			messages: `[{"role": "assistant", "content": "a", "name": "", "tool_calls": [], "refusal": null, "audio": null, "function_call": null}]`,
			want:     []provider.Message{{Role: provider.RoleAssistant, Parts: []string{"a"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustParse(t, body(t, map[string]string{"messages": tt.messages}))
			if !reflect.DeepEqual(got.Messages, tt.want) {
				t.Errorf("Messages = %+v, want %+v", got.Messages, tt.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name   string
		body   string // used as-is when params is nil
		params map[string]string
		code   string
		param  string
	}{
		// The body as a whole.
		{name: "empty body", body: ``, code: CodeInvalidJSON},
		{name: "malformed JSON", body: `{"model": `, code: CodeInvalidJSON},
		{name: "array body", body: `[]`, code: CodeInvalidJSON},
		{name: "null body", body: `null`, code: CodeInvalidJSON},
		{name: "string body", body: `"hi"`, code: CodeInvalidJSON},
		{name: "trailing data", body: `{"model": "m"} {}`, code: CodeInvalidJSON},
		{name: "unknown parameter", params: map[string]string{"temprature": `1`}, code: CodeUnknownParameter, param: "temprature"},

		// model
		{name: "model missing", params: map[string]string{"model": "-"}, code: CodeMissingParameter, param: "model"},
		{name: "model null", params: map[string]string{"model": `null`}, code: CodeMissingParameter, param: "model"},
		{name: "model empty", params: map[string]string{"model": `""`}, code: CodeMissingParameter, param: "model"},
		{name: "model not a string", params: map[string]string{"model": `5`}, code: CodeInvalidType, param: "model"},

		// messages
		{name: "messages missing", params: map[string]string{"messages": "-"}, code: CodeMissingParameter, param: "messages"},
		{name: "messages empty", params: map[string]string{"messages": `[]`}, code: CodeInvalidValue, param: "messages"},
		{name: "messages not an array", params: map[string]string{"messages": `"hi"`}, code: CodeInvalidType, param: "messages"},
		{name: "message not an object", params: map[string]string{"messages": `["hi"]`}, code: CodeInvalidType, param: "messages"},
		{name: "message null", params: map[string]string{"messages": `[null]`}, code: CodeInvalidType, param: "messages[0]"},
		{name: "role missing", params: map[string]string{"messages": `[{"content": "x"}]`}, code: CodeMissingParameter, param: "messages[0].role"},
		{name: "role not a string", params: map[string]string{"messages": `[{"role": 1, "content": "x"}]`}, code: CodeInvalidType, param: "messages[0].role"},
		{name: "role unknown", params: map[string]string{"messages": `[{"role": "bot", "content": "x"}]`}, code: CodeInvalidValue, param: "messages[0].role"},
		{name: "tool role", params: map[string]string{"messages": `[{"role": "tool", "content": "x", "tool_call_id": null}]`}, code: CodeUnsupportedValue, param: "messages[0].role"},
		{name: "function role", params: map[string]string{"messages": `[{"role": "function", "content": "x", "name": ""}]`}, code: CodeUnsupportedValue, param: "messages[0].role"},
		{name: "content missing", params: map[string]string{"messages": `[{"role": "user"}]`}, code: CodeMissingParameter, param: "messages[0].content"},
		{name: "content null", params: map[string]string{"messages": `[{"role": "assistant", "content": null}]`}, code: CodeMissingParameter, param: "messages[0].content"},
		{name: "content a number", params: map[string]string{"messages": `[{"role": "user", "content": 1}]`}, code: CodeInvalidType, param: "messages[0].content"},
		{name: "content empty array", params: map[string]string{"messages": `[{"role": "user", "content": []}]`}, code: CodeInvalidValue, param: "messages[0].content"},
		{name: "image part", params: map[string]string{"messages": `[{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "https://x"}}]}]`}, code: CodeUnsupportedValue, param: "messages[0].content[0].type"},
		{name: "audio part", params: map[string]string{"messages": `[{"role": "user", "content": [{"type": "input_audio", "input_audio": {}}]}]`}, code: CodeUnsupportedValue, param: "messages[0].content[0].type"},
		{name: "file part", params: map[string]string{"messages": `[{"role": "user", "content": [{"type": "file", "file": {}}]}]`}, code: CodeUnsupportedValue, param: "messages[0].content[0].type"},
		{name: "refusal part", params: map[string]string{"messages": `[{"role": "assistant", "content": [{"type": "refusal", "refusal": "no"}]}]`}, code: CodeUnsupportedValue, param: "messages[0].content[0].type"},
		{name: "part null", params: map[string]string{"messages": `[{"role": "user", "content": [null]}]`}, code: CodeInvalidType, param: "messages[0].content[0]"},
		{name: "part type missing", params: map[string]string{"messages": `[{"role": "user", "content": [{"text": "x"}]}]`}, code: CodeMissingParameter, param: "messages[0].content[0].type"},
		{name: "part text missing", params: map[string]string{"messages": `[{"role": "user", "content": [{"type": "text"}]}]`}, code: CodeMissingParameter, param: "messages[0].content[0].text"},
		{name: "part text not a string", params: map[string]string{"messages": `[{"role": "user", "content": [{"type": "text", "text": 1}]}]`}, code: CodeInvalidType, param: "messages[0].content[0].text"},
		{name: "part with extra field", params: map[string]string{"messages": `[{"role": "user", "content": [{"type": "text", "text": "x", "cache_control": {}}]}]`}, code: CodeUnknownParameter, param: "messages[0].content[0].cache_control"},
		{name: "message with unknown field", params: map[string]string{"messages": `[{"role": "user", "content": "x", "weight": 1}]`}, code: CodeUnknownParameter, param: "messages[0].weight"},
		{name: "message name", params: map[string]string{"messages": `[{"role": "user", "content": "x", "name": "bob"}]`}, code: CodeUnsupportedParameter, param: "messages[0].name"},
		{name: "message tool_calls", params: map[string]string{"messages": `[{"role": "assistant", "content": "x", "tool_calls": [{"id": "1"}]}]`}, code: CodeUnsupportedParameter, param: "messages[0].tool_calls"},
		{name: "message refusal", params: map[string]string{"messages": `[{"role": "assistant", "content": "x", "refusal": "no"}]`}, code: CodeUnsupportedParameter, param: "messages[0].refusal"},
		{name: "second message is checked", params: map[string]string{"messages": `[{"role": "user", "content": "x"}, {"role": "user"}]`}, code: CodeMissingParameter, param: "messages[1].content"},

		// stream and stream_options
		{name: "stream not a boolean", params: map[string]string{"stream": `"yes"`}, code: CodeInvalidType, param: "stream"},
		{name: "stream_options without stream", params: map[string]string{"stream_options": `{"include_usage": true}`}, code: CodeInvalidValue, param: "stream_options"},
		{name: "stream_options not an object", params: map[string]string{"stream": `true`, "stream_options": `true`}, code: CodeInvalidType, param: "stream_options"},
		{name: "include_usage not a boolean", params: map[string]string{"stream": `true`, "stream_options": `{"include_usage": 1}`}, code: CodeInvalidType, param: "stream_options.include_usage"},
		{name: "unknown stream option", params: map[string]string{"stream": `true`, "stream_options": `{"chunk_size": 1}`}, code: CodeUnknownParameter, param: "stream_options.chunk_size"},

		// token limits
		{name: "max_tokens zero", params: map[string]string{"max_tokens": `0`}, code: CodeInvalidValue, param: "max_tokens"},
		{name: "max_tokens negative", params: map[string]string{"max_tokens": `-5`}, code: CodeInvalidValue, param: "max_tokens"},
		{name: "max_tokens fractional", params: map[string]string{"max_tokens": `1.5`}, code: CodeInvalidType, param: "max_tokens"},
		{name: "max_tokens a string", params: map[string]string{"max_tokens": `"10"`}, code: CodeInvalidType, param: "max_tokens"},
		{name: "max_completion_tokens zero", params: map[string]string{"max_completion_tokens": `0`}, code: CodeInvalidValue, param: "max_completion_tokens"},
		{name: "both token limits", params: map[string]string{"max_tokens": `10`, "max_completion_tokens": `10`}, code: CodeInvalidValue, param: "max_tokens"},

		// sampling
		{name: "temperature below range", params: map[string]string{"temperature": `-0.1`}, code: CodeInvalidValue, param: "temperature"},
		{name: "temperature above range", params: map[string]string{"temperature": `2.1`}, code: CodeInvalidValue, param: "temperature"},
		{name: "temperature a string", params: map[string]string{"temperature": `"hot"`}, code: CodeInvalidType, param: "temperature"},
		{name: "top_p above range", params: map[string]string{"top_p": `1.1`}, code: CodeInvalidValue, param: "top_p"},
		{name: "top_p below range", params: map[string]string{"top_p": `-1`}, code: CodeInvalidValue, param: "top_p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := []byte(tt.body)
			if tt.params != nil {
				b = body(t, tt.params)
			}
			req, err := ParseChatCompletionRequest(b)
			if err == nil {
				t.Fatalf("ParseChatCompletionRequest(%s) = %+v, want an error", b, req)
			}
			wantError(t, err, tt.code, tt.param)
		})
	}
}

func TestErrorEnvelope(t *testing.T) {
	tests := []struct {
		err  *Error
		want string
	}{
		{
			&Error{Status: 400, Type: TypeInvalidRequest, Code: CodeInvalidValue, Param: "top_p", Message: "bad"},
			`{"error":{"message":"bad","type":"invalid_request_error","param":"top_p","code":"invalid_value"}}`,
		},
		{
			&Error{Status: 500, Type: TypeServer, Message: "oops"},
			`{"error":{"message":"oops","type":"server_error","param":null,"code":null}}`,
		},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.err.Envelope())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("envelope = %s, want %s", got, tt.want)
		}
	}
}

// Error messages name the parameter, so a client can find the problem.
func TestErrorMessagesNameTheParam(t *testing.T) {
	_, err := ParseChatCompletionRequest(body(t, map[string]string{"top_p": `7`}))
	if err == nil || !strings.Contains(err.Error(), "'top_p'") || !strings.Contains(err.Error(), "got 7") {
		t.Errorf("error = %v, want it to name 'top_p' and the bad value", err)
	}
}
