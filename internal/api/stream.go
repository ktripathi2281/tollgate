package api

import (
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// ChatCompletionChunk is one event of a streamed chat completion, in OpenAI's
// "chat.completion.chunk" shape.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is set only on the final usage chunk, whose Choices is empty.
	Usage *Usage `json:"usage,omitempty"`
}

// ChunkChoice is the one reply in a chunk.
type ChunkChoice struct {
	Index int   `json:"index"`
	Delta Delta `json:"delta"`
	// FinishReason is null until the chunk that ends the reply.
	FinishReason *string `json:"finish_reason"`
	// Logprobs is always null: log probabilities are not supported.
	Logprobs any `json:"logprobs"`
}

// Delta is what a chunk adds to the reply. Content is a pointer so that the
// first chunk can send an explicit empty string, as OpenAI does, while
// later chunks without content omit it.
type Delta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

// Chunks builds the chunks of one streamed completion. Every chunk of a
// stream carries the same ID, creation time and model.
type Chunks struct {
	ID      string
	Created int64
	Model   string
}

// NewChunks returns a builder for a stream with the given completion ID,
// creation time and upstream model.
func NewChunks(id string, created time.Time, model string) Chunks {
	return Chunks{ID: id, Created: created.Unix(), Model: model}
}

func (c Chunks) chunk(choices []ChunkChoice) ChatCompletionChunk {
	return ChatCompletionChunk{
		ID:      c.ID,
		Object:  "chat.completion.chunk",
		Created: c.Created,
		Model:   c.Model,
		Choices: choices,
	}
}

// Role is the first chunk: an empty reply from the assistant.
func (c Chunks) Role() ChatCompletionChunk {
	empty := ""
	return c.chunk([]ChunkChoice{{Delta: Delta{Role: string(provider.RoleAssistant), Content: &empty}}})
}

// Content is a chunk that adds text to the reply.
func (c Chunks) Content(text string) ChatCompletionChunk {
	return c.chunk([]ChunkChoice{{Delta: Delta{Content: &text}}})
}

// Finish is the chunk that ends the reply, with an empty delta.
func (c Chunks) Finish(reason provider.FinishReason) ChatCompletionChunk {
	r := string(reason)
	return c.chunk([]ChunkChoice{{FinishReason: &r}})
}

// Usage is the final chunk a client gets when it asked for usage: no
// choices, and the token counts.
func (c Chunks) Usage(u provider.Usage) ChatCompletionChunk {
	chunk := c.chunk([]ChunkChoice{})
	chunk.Usage = &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.PromptTokens + u.CompletionTokens,
	}
	return chunk
}
