package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

func fixedNow() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) }

// The chunks match OpenAI's shapes byte for byte, so SDKs parse them.
func TestChunkShapes(t *testing.T) {
	c := NewChunks("chatcmpl-1", fixedNow(), "mock-1")
	head := `"id":"chatcmpl-1","object":"chat.completion.chunk","created":1791450000,"model":"mock-1"`
	tests := []struct {
		name  string
		chunk ChatCompletionChunk
		want  string
	}{
		{
			"role", c.Role(),
			`{` + head + `,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null,"logprobs":null}]}`,
		},
		{
			"content", c.Content("Hello"),
			`{` + head + `,"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null,"logprobs":null}]}`,
		},
		{
			"finish", c.Finish(provider.FinishLength),
			`{` + head + `,"choices":[{"index":0,"delta":{},"finish_reason":"length","logprobs":null}]}`,
		},
		{
			"usage", c.Usage(provider.Usage{PromptTokens: 3, CompletionTokens: 4}),
			`{` + head + `,"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.chunk)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}
