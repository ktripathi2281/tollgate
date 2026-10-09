//go:build smoke

package anthropic

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// These tests call the real API, and only run with the smoke build tag:
// `make smoke`, which loads .env. They need ANTHROPIC_API_KEY and
// SMOKE_ANTHROPIC_MODEL, and are skipped without them.

func smokeAdapter(t *testing.T, key string) (*Provider, string) {
	t.Helper()
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}
	model := os.Getenv("SMOKE_ANTHROPIC_MODEL")
	if model == "" {
		t.Skip("SMOKE_ANTHROPIC_MODEL is not set")
	}
	client := provider.NewHTTPClient(10 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	return New(Config{Name: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: key, Client: client}), model
}

func smokeRequest(model string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model: model,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Parts: []string{"You answer in one word."}},
			{Role: provider.RoleUser, Parts: []string{"Reply with the single word: pong"}},
		},
		// Room for thinking, which recent models do by default, as well as
		// the reply.
		MaxTokens: 1024,
	}
}

func TestSmokeChat(t *testing.T) {
	p, model := smokeAdapter(t, os.Getenv("ANTHROPIC_API_KEY"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	resp, err := p.Chat(ctx, smokeRequest(model))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	t.Logf("model %s, finish %s, usage %+v, reply %q", resp.Model, resp.FinishReason, resp.Usage, resp.Content)
	if resp.Content == "" || resp.Usage == nil || resp.Usage.PromptTokens == 0 || resp.Usage.CompletionTokens == 0 {
		t.Error("want a reply and usage")
	}
}

func TestSmokeStream(t *testing.T) {
	p, model := smokeAdapter(t, os.Getenv("ANTHROPIC_API_KEY"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	s, err := p.ChatStream(ctx, smokeRequest(model))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer s.Close()
	chunks, err := drain(s)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text strings.Builder
	reasoning := 0
	var last *provider.Chunk
	for _, c := range chunks {
		text.WriteString(c.Delta)
		if c.Reasoning {
			reasoning++
		}
		last = c
	}
	t.Logf("%d chunks (%d reasoning), reply %q, final %+v usage %+v", len(chunks), reasoning, text.String(), last, last.Usage)
	if text.Len() == 0 || last.FinishReason == "" || last.Usage == nil || last.Usage.CompletionTokens == 0 {
		t.Error("want text, then a final chunk with the finish reason and usage")
	}
}

func TestSmokeErrors(t *testing.T) {
	p, _ := smokeAdapter(t, os.Getenv("ANTHROPIC_API_KEY"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	_, err := p.Chat(ctx, smokeRequest("tollgate-smoke-no-such-model"))
	if provider.ClassOf(err) != provider.AuthOrConfig {
		t.Errorf("unknown model: got %v (%v), want AuthOrConfig", err, provider.ClassOf(err))
	}

	bad, model := smokeAdapter(t, "sk-ant-tollgate-smoke-invalid")
	_, err = bad.Chat(ctx, smokeRequest(model))
	if provider.ClassOf(err) != provider.AuthOrConfig {
		t.Errorf("bad key: got %v (%v), want AuthOrConfig", err, provider.ClassOf(err))
	}
}
