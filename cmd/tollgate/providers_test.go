package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/anthropic"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
	"github.com/ktripathi2281/tollgate/internal/provider/openai"
)

func TestBuildProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.Provider{
		"mock":      {Type: "mock", OutputTokens: 1},
		"openai":    {Type: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		"anthropic": {Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"local":     {Type: "openai", BaseURL: "http://localhost:11434/v1", APIKeyEnv: "LOCAL_KEY"},
	}
	env := map[string]string{"OPENAI_API_KEY": "sk-test", "ANTHROPIC_API_KEY": "sk-ant-test"}
	var logs bytes.Buffer

	providers, err := buildProviders(cfg, func(k string) string { return env[k] }, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := providers["mock"].(*mock.Provider); !ok {
		t.Errorf("mock is %T", providers["mock"])
	}
	if _, ok := providers["openai"].(*openai.Provider); !ok {
		t.Errorf("openai is %T", providers["openai"])
	}
	if _, ok := providers["anthropic"].(*anthropic.Provider); !ok {
		t.Errorf("anthropic is %T", providers["anthropic"])
	}

	// No key for "local": disabled, not a startup failure.
	_, err = providers["local"].Chat(t.Context(), &provider.ChatRequest{})
	if provider.ClassOf(err) != provider.AuthOrConfig || !strings.Contains(err.Error(), "LOCAL_KEY is not set") {
		t.Errorf("local: got %v, want a disabled provider naming LOCAL_KEY", err)
	}
	if !strings.Contains(logs.String(), `"msg":"provider disabled"`) || !strings.Contains(logs.String(), "LOCAL_KEY") {
		t.Errorf("no startup warning for the disabled provider:\n%s", logs.String())
	}
	// The keys themselves never reach the log.
	for _, key := range env {
		if strings.Contains(logs.String(), key) {
			t.Error("an API key was logged")
		}
	}
}
