package main

import (
	"fmt"
	"log/slog"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/anthropic"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
	"github.com/ktripathi2281/tollgate/internal/provider/openai"
)

// buildProviders creates one provider per config entry. getenv reads the
// environment, where API keys live; tests pass a fake one.
//
// A real provider whose key variable isn't set is disabled rather than
// stopping startup: the mock aliases still work with no keys at all, and
// requests for aliases that use it fall back past it, logged loudly.
func buildProviders(cfg *config.Config, getenv func(string) string, log *slog.Logger) (map[string]provider.Provider, error) {
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for name, c := range cfg.Providers {
		switch c.Type {
		case config.TypeMock:
			providers[name] = mock.New(name, mock.Config{
				TTFT:          c.TTFT,
				TokenInterval: c.TokenInterval,
				OutputTokens:  c.OutputTokens,
				ErrorRate:     c.ErrorRate,
				Status:        c.Status,
				Hang:          c.Hang,
				Seed:          c.Seed,
				FailAtChunk:   c.FailAtChunk,
				StallAtChunk:  c.StallAtChunk,
			})

		case config.TypeOpenAI, config.TypeAnthropic:
			key := getenv(c.APIKeyEnv)
			if key == "" {
				reason := c.APIKeyEnv + " is not set"
				log.Warn("provider disabled", "provider", name, "reason", reason)
				providers[name] = provider.Disabled(name, reason)
				continue
			}
			// One client per provider, so each gets its own connection pool.
			client := provider.NewHTTPClient(cfg.Timeouts.Connect)
			if c.Type == config.TypeOpenAI {
				providers[name] = openai.New(openai.Config{
					Name: name, BaseURL: c.BaseURL, APIKey: key, TokenParam: c.TokenParam, Client: client,
				})
			} else {
				providers[name] = anthropic.New(anthropic.Config{
					Name: name, BaseURL: c.BaseURL, APIKey: key, Client: client,
				})
			}

		default:
			// Config validation rejects unknown types, so this is a bug.
			return nil, fmt.Errorf("provider %s: unknown type %q", name, c.Type)
		}
	}
	return providers, nil
}
