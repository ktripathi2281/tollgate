// Package openai is the adapter for OpenAI's Chat Completions API, and for
// any server that speaks the same format (vLLM, Ollama and others), chosen
// by base URL. It uses net/http directly, so the wire format stays visible.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// The two names OpenAI-compatible servers use for the output token limit.
const (
	TokenParamMaxCompletionTokens = "max_completion_tokens"
	TokenParamMaxTokens           = "max_tokens"
)

// maxResponseBytes caps a non-streaming response body. A reply is bounded
// by its token limit, so this only guards against a broken upstream.
const maxResponseBytes = 16 << 20

// Config configures one OpenAI-compatible provider.
type Config struct {
	Name    string // the provider's name in the gateway config
	BaseURL string // such as https://api.openai.com/v1
	APIKey  string
	// TokenParam is the request field for the token limit:
	// TokenParamMaxCompletionTokens (the default) or TokenParamMaxTokens.
	TokenParam string
	Client     *http.Client // shared by every call; see provider.NewHTTPClient
}

// Provider is the OpenAI-compatible adapter. It is safe for concurrent use.
type Provider struct {
	cfg      Config
	endpoint string // the chat completions URL
}

var _ provider.Provider = (*Provider)(nil)

// New returns an adapter for cfg.
func New(cfg Config) *Provider {
	if cfg.TokenParam == "" {
		cfg.TokenParam = TokenParamMaxCompletionTokens
	}
	return &Provider{cfg: cfg, endpoint: strings.TrimSuffix(cfg.BaseURL, "/") + "/chat/completions"}
}

// Name returns the provider's name.
func (p *Provider) Name() string { return p.cfg.Name }

// Chat sends a non-streaming request.
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, err := p.post(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, p.badResponse(ctx, "decode response", err)
	}
	if len(out.Choices) == 0 {
		return nil, p.badResponse(ctx, "response has no choices", nil)
	}
	choice := out.Choices[0]

	reply := &provider.ChatResponse{
		Model:        out.Model,
		Content:      deref(choice.Message.Content),
		FinishReason: finishReason(choice.FinishReason),
		Usage:        out.Usage.canonical(),
	}
	if reply.Model == "" {
		reply.Model = req.Model
	}
	// A refusal comes back with no content and the refusal text in its own
	// field. The gateway passes the text on as the reply, marked as filtered.
	if reply.Content == "" && choice.Message.Refusal != nil {
		reply.Content = *choice.Message.Refusal
		reply.FinishReason = provider.FinishContentFilter
	}
	return reply, nil
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
	body, err := json.Marshal(p.wireRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", p.cfg.Name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.cfg.Name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
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

// statusError classifies a non-2xx response from the status code, the
// error body and the Retry-After header.
func (p *Provider) statusError(resp *http.Response) error {
	var body errorBody
	_ = json.Unmarshal(provider.ReadErrorBody(resp.Body), &body)

	msg := body.Error.Message
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	class := provider.ClassifyStatus(resp.StatusCode)
	// Some 429s are about money, not pace: the account is out of credit or
	// over a spend limit. Waiting doesn't fix those, so they are a config
	// problem for the gateway, not a rate limit to retry.
	if resp.StatusCode == http.StatusTooManyRequests && body.isQuota() {
		class = provider.AuthOrConfig
	}
	return &provider.Error{
		Provider:   p.cfg.Name,
		Class:      class,
		Status:     resp.StatusCode,
		Message:    msg,
		RetryAfter: provider.RetryAfter(resp.Header, time.Now()),
	}
}

// badResponse reports a 2xx response the adapter couldn't use. That is the
// upstream misbehaving, so it is Unavailable, unless the read failed
// because ctx ended.
func (p *Provider) badResponse(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %s: %w", p.cfg.Name, what, ctx.Err())
	}
	return &provider.Error{Provider: p.cfg.Name, Class: provider.Unavailable, Message: what, Err: err}
}

// chatRequest is the request body the adapter sends.
type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []message      `json:"messages"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	TopP                *float64       `json:"top_p,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
}

type message struct {
	Role string `json:"role"`
	// Content is a string for a one-part message, or a []textPart.
	Content any `json:"content"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireRequest translates the canonical request. A stream always asks for
// usage, whether or not the client did: the gateway needs it to bill.
func (p *Provider) wireRequest(req *provider.ChatRequest, stream bool) chatRequest {
	out := chatRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      stream,
	}
	if p.cfg.TokenParam == TokenParamMaxTokens {
		out.MaxTokens = req.MaxTokens
	} else {
		out.MaxCompletionTokens = req.MaxTokens
	}
	if stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		var content any = ""
		switch len(m.Parts) {
		case 0:
		case 1:
			content = m.Parts[0]
		default:
			parts := make([]textPart, len(m.Parts))
			for i, text := range m.Parts {
				parts[i] = textPart{Type: "text", Text: text}
			}
			content = parts
		}
		out.Messages = append(out.Messages, message{Role: string(m.Role), Content: content})
	}
	return out
}

// chatResponse is the part of a chat.completion the adapter reads. Fields it
// doesn't model, such as system_fingerprint, are ignored.
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

// usage is OpenAI's token count. completion_tokens includes any hidden
// reasoning tokens, which are billed as output.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *usage) canonical() *provider.Usage {
	if u == nil {
		return nil
	}
	return &provider.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens}
}

// errorBody is OpenAI's error envelope. code is a string in practice, but
// is read loosely so an odd value can't break error handling.
type errorBody struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// quotaCodes are the 429 codes that mean the account can't pay, not that it
// is going too fast.
var quotaCodes = []string{
	"insufficient_quota",
	"credit_balance_exhausted",
	"organization_spend_limit_exceeded",
	"project_spend_limit_exceeded",
	"organization_usage_limit_exceeded",
}

func (b errorBody) isQuota() bool {
	var code string
	_ = json.Unmarshal(b.Error.Code, &code)
	return b.Error.Type == "insufficient_quota" || slices.Contains(quotaCodes, code)
}

// finishReason maps OpenAI's finish reasons. tool_calls and function_call
// can't happen, because the gateway sends no tools; anything unknown is
// treated as a natural stop.
func finishReason(r string) provider.FinishReason {
	switch r {
	case "length":
		return provider.FinishLength
	case "content_filter":
		return provider.FinishContentFilter
	default:
		return provider.FinishStop
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// errStreamEnded means the stream stopped before OpenAI's closing [DONE].
var errStreamEnded = errors.New("stream ended before [DONE]")
