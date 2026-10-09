//go:build smoke

package openai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// These tests call the real API, and only run with the smoke build tag:
// `make smoke`, which loads .env. They need OPENAI_API_KEY and
// SMOKE_OPENAI_MODEL, and are skipped without them. SMOKE_OPENAI_BASE_URL
// points them at another OpenAI-compatible server.

func smokeAdapter(t *testing.T, key string) (*Provider, string) {
	t.Helper()
	if key == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}
	model := os.Getenv("SMOKE_OPENAI_MODEL")
	if model == "" {
		t.Skip("SMOKE_OPENAI_MODEL is not set")
	}
	baseURL := os.Getenv("SMOKE_OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	client := provider.NewHTTPClient(10 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	return New(Config{Name: "openai", BaseURL: baseURL, APIKey: key, Client: client}), model
}

func smokeRequest(model string) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: provider.RoleUser, Parts: []string{"Reply with the single word: pong"}}},
		// Room for a reasoning model's hidden reasoning as well as the reply.
		MaxTokens: 512,
	}
}

func TestSmokeChat(t *testing.T) {
	p, model := smokeAdapter(t, os.Getenv("OPENAI_API_KEY"))
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
	p, model := smokeAdapter(t, os.Getenv("OPENAI_API_KEY"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	s, err := p.ChatStream(ctx, smokeRequest(model))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text strings.Builder
	var finish provider.FinishReason
	var usage *provider.Usage
	for _, c := range chunks {
		text.WriteString(c.Delta)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	t.Logf("%d chunks, finish %s, usage %+v, reply %q", len(chunks), finish, usage, text.String())
	if text.Len() == 0 || finish == "" || usage == nil {
		t.Error("want text, a finish reason and usage")
	}
}

func TestSmokeErrors(t *testing.T) {
	p, _ := smokeAdapter(t, os.Getenv("OPENAI_API_KEY"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// A model ID that doesn't exist is the gateway's config problem.
	_, err := p.Chat(ctx, smokeRequest("tollgate-smoke-no-such-model"))
	if provider.ClassOf(err) != provider.AuthOrConfig {
		t.Errorf("unknown model: got %v (%v), want AuthOrConfig", err, provider.ClassOf(err))
	}

	// So is a wrong key.
	bad, model := smokeAdapter(t, "sk-tollgate-smoke-invalid")
	_, err = bad.Chat(ctx, smokeRequest(model))
	if provider.ClassOf(err) != provider.AuthOrConfig {
		t.Errorf("bad key: got %v (%v), want AuthOrConfig", err, provider.ClassOf(err))
	}
}
