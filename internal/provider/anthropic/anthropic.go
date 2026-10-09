// Package anthropic is the adapter for Anthropic's Messages API. It
// translates the gateway's OpenAI-shaped requests into Messages requests,
// and Anthropic's responses and stream events back. It uses net/http
// directly, so the wire format stays visible.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// APIVersion is the anthropic-version header the adapter is written against.
const APIVersion = "2023-06-01"

// maxResponseBytes caps a non-streaming response body. A reply is bounded
// by its token limit, so this only guards against a broken upstream.
const maxResponseBytes = 16 << 20

// Config configures the adapter.
type Config struct {
	Name    string // the provider's name in the gateway config
	BaseURL string // such as https://api.anthropic.com
	APIKey  string
	Client  *http.Client // shared by every call; see provider.NewHTTPClient
}

// Provider is the Anthropic adapter. It is safe for concurrent use.
type Provider struct {
	cfg      Config
	endpoint string // the Messages API URL
}

var _ provider.Provider = (*Provider)(nil)

// New returns an adapter for cfg.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg, endpoint: strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/messages"}
}

// Name returns the provider's name.
func (p *Provider) Name() string { return p.cfg.Name }

// Validate checks the rules a request must meet for Anthropic that the
// OpenAI format doesn't have. The router calls it for every target of an
// alias before the first attempt, so a fallback to Anthropic never turns a
// valid request into an upstream 400.
func (p *Provider) Validate(req *provider.ChatRequest) error {
	if t := req.Temperature; t != nil && (*t < 0 || *t > 1) {
		return &provider.ParamError{Param: "temperature", Message: fmt.Sprintf("must be from 0 to 1 for Anthropic models, got %g", *t)}
	}
	if req.Temperature != nil && req.TopP != nil {
		return &provider.ParamError{Param: "top_p", Message: "Anthropic models take temperature or top_p, not both"}
	}
	conversation := 0
	for _, m := range req.Messages {
		if m.Role == provider.RoleSystem || m.Role == provider.RoleDeveloper {
			continue
		}
		conversation++
		if !hasText(m.Parts) {
			return &provider.ParamError{Param: "messages", Message: "Anthropic models need text in every user and assistant message"}
		}
	}
	if conversation == 0 {
		return &provider.ParamError{Param: "messages", Message: "Anthropic models need at least one user or assistant message"}
	}
	return nil
}

func hasText(parts []string) bool {
	for _, part := range parts {
		if part != "" {
			return true
		}
	}
	return false
}

// Chat sends a non-streaming request.
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, err := p.post(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out messageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: decode response: %w", p.cfg.Name, ctx.Err())
		}
		return nil, &provider.Error{Provider: p.cfg.Name, Class: provider.Unavailable, Message: "decode response", Err: err}
	}

	// The reply is the text blocks in order. Thinking blocks, and anything
	// else the gateway doesn't model, are left out.
	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	model := out.Model
	if model == "" {
		model = req.Model
	}
	return &provider.ChatResponse{
		Model:        model,
		Content:      text.String(),
		FinishReason: finishReason(out.StopReason),
		Usage:        &provider.Usage{PromptTokens: out.Usage.prompt(), CompletionTokens: out.Usage.OutputTokens},
	}, nil
}

// ChatStream sends a streaming request and returns once the response
// headers have arrived.
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.Stream, error) {
	resp, err := p.post(ctx, req, true) //nolint:bodyclose // the returned stream owns the body and closes it in Close
	if err != nil {
		return nil, err
	}
	return newStream(ctx, p.cfg.Name, resp.Body), nil
}

// post sends req and returns the response if its status is 2xx. Anything
// else becomes a classified *provider.Error.
func (p *Provider) post(ctx context.Context, req *provider.ChatRequest, stream bool) (*http.Response, error) {
	body, err := json.Marshal(wireRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", p.cfg.Name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.cfg.Name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", p.cfg.APIKey)
	httpReq.Header.Set("Anthropic-Version", APIVersion)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.cfg.Client.Do(httpReq)
	if err != nil {
		return nil, provider.TransportError(ctx, p.cfg.Name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, p.statusError(resp)
	}
	return resp, nil
}

// statusError classifies a non-2xx response. Anthropic's 529 (overloaded)
// and 504 (timeout) are Unavailable like any 5xx, 402 (billing) is a config
// error, and 429 is a rate limit.
func (p *Provider) statusError(resp *http.Response) error {
	var body errorBody
	_ = json.Unmarshal(provider.ReadErrorBody(resp.Body), &body)
	msg := body.Error.Message
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &provider.Error{
		Provider:   p.cfg.Name,
		Class:      provider.ClassifyStatus(resp.StatusCode),
		Status:     resp.StatusCode,
		Message:    msg,
		RetryAfter: provider.RetryAfter(resp.Header, time.Now()),
	}
}

// messagesRequest is the request body the adapter sends.
type messagesRequest struct {
	Model         string      `json:"model"`
	MaxTokens     int         `json:"max_tokens"`
	System        []textBlock `json:"system,omitempty"`
	Messages      []message   `json:"messages"`
	Temperature   *float64    `json:"temperature,omitempty"`
	TopP          *float64    `json:"top_p,omitempty"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
}

type message struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// wireRequest translates the canonical request. System and developer
// messages, wherever they appear, become the top-level system prompt, in
// order; Anthropic has no system role inside the conversation. Each text
// part becomes its own block, and empty parts are dropped, because
// Anthropic rejects empty text blocks.
func wireRequest(req *provider.ChatRequest, stream bool) messagesRequest {
	out := messagesRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Stream:        stream,
		Messages:      []message{},
	}
	for _, m := range req.Messages {
		blocks := textBlocks(m.Parts)
		switch m.Role {
		case provider.RoleSystem, provider.RoleDeveloper:
			out.System = append(out.System, blocks...)
		default:
			out.Messages = append(out.Messages, message{Role: string(m.Role), Content: blocks})
		}
	}
	return out
}

func textBlocks(parts []string) []textBlock {
	blocks := make([]textBlock, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			blocks = append(blocks, textBlock{Type: "text", Text: part})
		}
	}
	return blocks
}

// messageResponse is the part of a Messages response the adapter reads.
type messageResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      usage  `json:"usage"`
}

// usage is Anthropic's token count. input_tokens excludes tokens written to
// or read from the prompt cache, which are counted separately.
type usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// prompt is every input token, cached or not. Cache discounts aren't
// modelled (a documented limitation), so all of them are billed as input.
func (u usage) prompt() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// finishReason maps Anthropic's stop reasons onto OpenAI's finish reasons.
func finishReason(r string) provider.FinishReason {
	switch r {
	case "max_tokens", "model_context_window_exceeded":
		return provider.FinishLength
	case "refusal":
		return provider.FinishContentFilter
	default:
		// end_turn and stop_sequence are natural stops. tool_use and
		// pause_turn need tools, which the gateway never sends, and an
		// unknown reason is treated as a natural stop too.
		return provider.FinishStop
	}
}

// errStreamEnded means the stream stopped before message_stop.
var errStreamEnded = errors.New("stream ended before message_stop")
