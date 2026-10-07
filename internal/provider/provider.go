// Package provider defines the gateway's canonical chat types and the
// interface every upstream adapter implements.
//
// The canonical types are the gateway's own, not any provider's wire format.
// Each adapter translates them to and from its provider's API, so the rest
// of the gateway never sees a provider-specific field.
package provider

import "context"

// Provider is an upstream LLM provider.
type Provider interface {
	// Name is the provider's name in the config, such as "mock".
	Name() string
	// Chat sends a request and waits for the complete response. It returns
	// an error that ClassOf can classify.
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}

// Role is the author of a message.
type Role string

// The roles the gateway supports.
const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one message in a conversation.
type Message struct {
	Role Role
	// Parts holds the message's text in order. Content sent as a plain
	// string becomes one part; an array of text parts keeps its parts, so
	// adapters can map them one to one.
	Parts []string
}

// ChatRequest is a chat completion request for one upstream target.
type ChatRequest struct {
	// Model is the upstream model ID, for example "mock-1".
	Model    string
	Messages []Message
	// MaxTokens is always set: the gateway injects the alias default when
	// the client sends no limit, so every request has a real upper bound.
	MaxTokens   int
	Temperature *float64 // nil means the provider's default
	TopP        *float64 // nil means the provider's default
	Stop        []string
}

// FinishReason says why generation stopped.
type FinishReason string

// The finish reasons the gateway reports, using OpenAI's names.
const (
	FinishStop          FinishReason = "stop"           // natural end or a stop sequence
	FinishLength        FinishReason = "length"         // MaxTokens reached
	FinishContentFilter FinishReason = "content_filter" // output withheld by the provider
)

// Usage is the token count for one call, as reported by the provider.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// ChatResponse is a complete, non-streaming response.
type ChatResponse struct {
	// Model is the upstream model that produced the response.
	Model        string
	Content      string
	FinishReason FinishReason
	Usage        Usage
}
