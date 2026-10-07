package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// ChatCompletionRequest is a validated chat completion request: the subset
// of OpenAI's request that the gateway supports.
type ChatCompletionRequest struct {
	// Model is the alias the client asked for.
	Model    string
	Messages []provider.Message
	Stream   bool
	// IncludeUsage is stream_options.include_usage.
	IncludeUsage bool
	// MaxTokens is max_tokens or max_completion_tokens, whichever was sent,
	// or 0 if the client set no limit.
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stop        []string
}

// Top-level request parameters fall into three groups. Every parameter in
// OpenAI's API reference is in exactly one of them (a test checks this), and
// anything else is rejected as unknown.

// supportedParams are decoded and validated.
var supportedParams = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "stop": true,
}

// ignoredParams are accepted and dropped. None of them changes what the
// model generates: they affect storage, caching, billing tier, latency or
// abuse tracking on the provider's side.
var ignoredParams = map[string]bool{
	"user": true, "metadata": true, "store": true, "safety_identifier": true,
	"prompt_cache_key": true, "prompt_cache_options": true, "prompt_cache_retention": true,
	"service_tier": true, "parallel_tool_calls": true, "prediction": true,
}

// restriction describes a parameter the gateway doesn't support. Dropping it
// silently would change the output, so it is rejected unless it is null or
// has the value that changes nothing.
type restriction struct {
	// allows reports whether a value is the no-op value. nil means only
	// null is accepted.
	allows func(json.RawMessage) bool
	// noop describes the accepted value, for the error message.
	noop string
}

var restrictedParams = map[string]restriction{
	"n":                  {number(1), "1"},
	"logprobs":           {literal(`false`), "false"},
	"top_logprobs":       {number(0), "0"},
	"presence_penalty":   {number(0), "0"},
	"frequency_penalty":  {number(0), "0"},
	"logit_bias":         {literal(`{}`), "{}"},
	"tools":              {literal(`[]`), "[]"},
	"functions":          {literal(`[]`), "[]"},
	"tool_choice":        {literal(`"none"`), `"none"`},
	"function_call":      {literal(`"none"`), `"none"`},
	"response_format":    {literal(`{"type":"text"}`), `{"type": "text"}`},
	"modalities":         {literal(`["text"]`), `["text"]`},
	"seed":               {},
	"audio":              {},
	"reasoning_effort":   {},
	"verbosity":          {},
	"web_search_options": {},
	"moderation":         {},
}

// Limits on supported parameters, from OpenAI's API reference. Narrower
// limits for particular providers are applied per alias (brief section 7).
const (
	minTemperature = 0
	maxTemperature = 2
	minTopP        = 0
	maxTopP        = 1
)

// ParseChatCompletionRequest parses and validates a request body. Any error
// it returns is an *Error describing the problem for the client.
func ParseChatCompletionRequest(body []byte) (*ChatCompletionRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, invalidf("", CodeInvalidJSON,
			"The request body is not a valid JSON object.")
	}

	// Check which parameters were sent before decoding any of them. Sorted
	// order means the same request always gets the same error.
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if err := checkParam(name, fields[name]); err != nil {
			return nil, err
		}
	}

	req := &ChatCompletionRequest{}
	var err error

	raw, ok := lookup(fields, "model")
	if !ok {
		return nil, invalidf("model", CodeMissingParameter, "You must provide a model parameter.")
	}
	if req.Model, err = decode[string](raw, "model", "a string"); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, invalidf("model", CodeMissingParameter, "You must provide a model parameter.")
	}

	if req.Messages, err = parseMessages(fields); err != nil {
		return nil, err
	}

	if raw, ok := lookup(fields, "stream"); ok {
		if req.Stream, err = decode[bool](raw, "stream", "a boolean"); err != nil {
			return nil, err
		}
	}
	if raw, ok := lookup(fields, "stream_options"); ok {
		if !req.Stream {
			return nil, invalidf("stream_options", CodeInvalidValue,
				"Invalid 'stream_options': only allowed when 'stream' is true.")
		}
		if req.IncludeUsage, err = parseStreamOptions(raw); err != nil {
			return nil, err
		}
	}

	if req.MaxTokens, err = parseMaxTokens(fields); err != nil {
		return nil, err
	}
	if req.Temperature, err = parseRange(fields, "temperature", minTemperature, maxTemperature); err != nil {
		return nil, err
	}
	if req.TopP, err = parseRange(fields, "top_p", minTopP, maxTopP); err != nil {
		return nil, err
	}

	if raw, ok := lookup(fields, "stop"); ok {
		if req.Stop, err = parseStop(raw); err != nil {
			return nil, invalidf("stop", CodeInvalidValue, "Invalid 'stop': %v.", err)
		}
	}
	return req, nil
}

// checkParam accepts supported and ignored parameters, accepts a restricted
// parameter only with a value that changes nothing, and rejects the rest.
func checkParam(name string, raw json.RawMessage) error {
	if supportedParams[name] || ignoredParams[name] {
		return nil
	}
	r, ok := restrictedParams[name]
	if !ok {
		return invalidf(name, CodeUnknownParameter, "Unrecognized request argument supplied: %s", name)
	}
	if isNull(raw) || (r.allows != nil && r.allows(raw)) {
		return nil
	}
	if r.allows == nil {
		return invalidf(name, CodeUnsupportedParameter,
			"Unsupported parameter: '%s' is not supported by this gateway.", name)
	}
	return invalidf(name, CodeUnsupportedValue,
		"Unsupported value for '%s': this gateway only accepts %s.", name, r.noop)
}

// parseMessages decodes and validates the "messages" array.
func parseMessages(fields map[string]json.RawMessage) ([]provider.Message, error) {
	raw, ok := lookup(fields, "messages")
	if !ok {
		return nil, invalidf("messages", CodeMissingParameter, "You must provide a messages parameter.")
	}
	items, err := decode[[]map[string]json.RawMessage](raw, "messages", "an array of message objects")
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, invalidf("messages", CodeInvalidValue, "Invalid 'messages': expected at least one message.")
	}
	messages := make([]provider.Message, 0, len(items))
	for i, item := range items {
		msg, err := parseMessage(fmt.Sprintf("messages[%d]", i), item)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

// messageRestrictions are message fields the gateway doesn't support. As at
// the top level, each is accepted only when it is null or a no-op value.
var messageRestrictions = map[string]restriction{
	"name":          {literal(`""`), `""`},
	"tool_calls":    {literal(`[]`), "[]"},
	"tool_call_id":  {},
	"function_call": {},
	"refusal":       {},
	"audio":         {},
}

var supportedRoles = map[provider.Role]bool{
	provider.RoleSystem: true, provider.RoleDeveloper: true,
	provider.RoleUser: true, provider.RoleAssistant: true,
}

// parseMessage decodes one message. path is its parameter path, such as
// "messages[2]".
func parseMessage(path string, fields map[string]json.RawMessage) (provider.Message, error) {
	if fields == nil {
		return provider.Message{}, invalidf(path, CodeInvalidType, "Invalid type for '%s': expected an object.", path)
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if name == "role" || name == "content" {
			continue
		}
		param := path + "." + name
		r, ok := messageRestrictions[name]
		if !ok {
			return provider.Message{}, invalidf(param, CodeUnknownParameter,
				"Unrecognized message field supplied: %s", param)
		}
		if raw := fields[name]; !isNull(raw) && (r.allows == nil || !r.allows(raw)) {
			return provider.Message{}, invalidf(param, CodeUnsupportedParameter,
				"Unsupported parameter: '%s' is not supported by this gateway.", param)
		}
	}

	raw, ok := lookup(fields, "role")
	if !ok {
		return provider.Message{}, invalidf(path+".role", CodeMissingParameter, "Missing '%s.role'.", path)
	}
	role, err := decode[provider.Role](raw, path+".role", "a string")
	if err != nil {
		return provider.Message{}, err
	}
	switch {
	case role == "tool" || role == "function":
		return provider.Message{}, invalidf(path+".role", CodeUnsupportedValue,
			"Unsupported value for '%s.role': %s messages are not supported, because tool calling is not.", path, role)
	case !supportedRoles[role]:
		return provider.Message{}, invalidf(path+".role", CodeInvalidValue,
			"Invalid '%s.role': must be one of system, developer, user, assistant; got %q.", path, role)
	}

	raw, ok = lookup(fields, "content")
	if !ok {
		return provider.Message{}, invalidf(path+".content", CodeMissingParameter, "Missing '%s.content'.", path)
	}
	parts, err := parseContent(path+".content", raw)
	if err != nil {
		return provider.Message{}, err
	}
	return provider.Message{Role: role, Parts: parts}, nil
}

// parseContent decodes message content: a string, or an array of text parts.
func parseContent(param string, raw json.RawMessage) ([]string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []string{text}, nil
	}
	parts, err := decode[[]map[string]json.RawMessage](raw, param, "a string or an array of text parts")
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, invalidf(param, CodeInvalidValue, "Invalid '%s': expected at least one content part.", param)
	}
	texts := make([]string, 0, len(parts))
	for i, part := range parts {
		path := fmt.Sprintf("%s[%d]", param, i)
		if part == nil {
			return nil, invalidf(path, CodeInvalidType, "Invalid type for '%s': expected an object.", path)
		}
		raw, ok := lookup(part, "type")
		if !ok {
			return nil, invalidf(path+".type", CodeMissingParameter, "Missing '%s.type'.", path)
		}
		typ, err := decode[string](raw, path+".type", "a string")
		if err != nil {
			return nil, err
		}
		if typ != "text" {
			return nil, invalidf(path+".type", CodeUnsupportedValue,
				"Unsupported value for '%s.type': only text content parts are supported; got %q.", path, typ)
		}
		for _, name := range slices.Sorted(maps.Keys(part)) {
			if name != "type" && name != "text" {
				return nil, invalidf(path+"."+name, CodeUnknownParameter,
					"Unrecognized content part field supplied: %s.%s", path, name)
			}
		}
		raw, ok = lookup(part, "text")
		if !ok {
			return nil, invalidf(path+".text", CodeMissingParameter, "Missing '%s.text'.", path)
		}
		t, err := decode[string](raw, path+".text", "a string")
		if err != nil {
			return nil, err
		}
		texts = append(texts, t)
	}
	return texts, nil
}

// parseStreamOptions decodes stream_options and returns include_usage.
func parseStreamOptions(raw json.RawMessage) (bool, error) {
	opts, err := decode[map[string]json.RawMessage](raw, "stream_options", "an object")
	if err != nil {
		return false, err
	}
	var includeUsage bool
	for _, name := range slices.Sorted(maps.Keys(opts)) {
		param := "stream_options." + name
		switch name {
		case "include_usage":
			if raw, ok := lookup(opts, name); ok {
				if includeUsage, err = decode[bool](raw, param, "a boolean"); err != nil {
					return false, err
				}
			}
		case "include_obfuscation":
			// Ignored: it only pads stream events, and the gateway writes
			// its own events.
		default:
			return false, invalidf(param, CodeUnknownParameter, "Unrecognized request argument supplied: %s", param)
		}
	}
	return includeUsage, nil
}

// parseMaxTokens accepts max_tokens or max_completion_tokens (OpenAI's newer
// name for the same limit), but not both.
func parseMaxTokens(fields map[string]json.RawMessage) (int, error) {
	rawOld, hasOld := lookup(fields, "max_tokens")
	rawNew, hasNew := lookup(fields, "max_completion_tokens")
	if hasOld && hasNew {
		return 0, invalidf("max_tokens", CodeInvalidValue,
			"Set either 'max_tokens' or 'max_completion_tokens', not both.")
	}
	param, raw := "max_tokens", rawOld
	if hasNew {
		param, raw = "max_completion_tokens", rawNew
	} else if !hasOld {
		return 0, nil
	}
	n, err := decode[int](raw, param, "an integer")
	if err != nil {
		return 0, err
	}
	if n < 1 {
		return 0, invalidf(param, CodeInvalidValue, "Invalid '%s': must be at least 1, got %d.", param, n)
	}
	return n, nil
}

// parseRange decodes an optional number and checks it is within [lo, hi].
func parseRange(fields map[string]json.RawMessage, name string, lo, hi float64) (*float64, error) {
	raw, ok := lookup(fields, name)
	if !ok {
		return nil, nil
	}
	v, err := decode[float64](raw, name, "a number")
	if err != nil {
		return nil, err
	}
	if v < lo || v > hi {
		return nil, invalidf(name, CodeInvalidValue, "Invalid '%s': must be from %g to %g, got %g.", name, lo, hi, v)
	}
	return &v, nil
}

// lookup returns a field's raw value. It reports false if the field is
// absent or null: OpenAI treats an explicit null as "not set".
func lookup(fields map[string]json.RawMessage, name string) (json.RawMessage, bool) {
	raw, ok := fields[name]
	if !ok || isNull(raw) {
		return nil, false
	}
	return raw, true
}

// decode unmarshals raw into a T. If raw has the wrong JSON type, the error
// names param and what was expected.
func decode[T any](raw json.RawMessage, param, expected string) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, invalidf(param, CodeInvalidType, "Invalid type for '%s': expected %s.", param, expected)
	}
	return v, nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// number returns a check that a value is the JSON number want, in any
// spelling: 0, 0.0 and 0e0 are all zero.
func number(want float64) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var v float64
		return json.Unmarshal(raw, &v) == nil && v == want
	}
}

// literal returns a check that a value is the JSON text want, ignoring
// insignificant whitespace.
func literal(want string) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return false
		}
		return compact.String() == want
	}
}
