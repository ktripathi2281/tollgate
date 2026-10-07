package api

import (
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// ChatCompletion is a non-streaming chat completion response, in OpenAI's
// "chat.completion" shape.
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one generated reply. The gateway only supports n=1, so a
// response always has exactly one, at index 0.
type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
	// Logprobs is always null: log probabilities are not supported.
	Logprobs any `json:"logprobs"`
}

// ResponseMessage is the assistant's reply.
type ResponseMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Refusal is always null. A provider refusal is returned as content.
	Refusal *string `json:"refusal"`
}

// Usage is the token count for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// NewChatCompletion builds the response for a provider result. id is the
// gateway's own completion ID, and created is when the request arrived.
func NewChatCompletion(id string, created time.Time, resp *provider.ChatResponse) ChatCompletion {
	return ChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created.Unix(),
		Model:   resp.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      ResponseMessage{Role: string(provider.RoleAssistant), Content: resp.Content},
			FinishReason: string(resp.FinishReason),
		}},
		Usage: Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.PromptTokens + resp.Usage.CompletionTokens,
		},
	}
}

// ModelList is the response to GET /v1/models.
type ModelList struct {
	Object string  `json:"object"` // always "list"
	Data   []Model `json:"data"`
}

// Model is one entry in a ModelList. For the gateway, a model is an alias.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // always "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// NewModelList lists aliases in the given order. created is reported as
// every model's creation time.
func NewModelList(aliases []string, created time.Time) ModelList {
	list := ModelList{Object: "list", Data: make([]Model, 0, len(aliases))}
	for _, alias := range aliases {
		list.Data = append(list.Data, Model{
			ID:      alias,
			Object:  "model",
			Created: created.Unix(),
			OwnedBy: "system",
		})
	}
	return list
}
