package main

import (
	"fmt"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

// buildProviders creates one provider per config entry.
func buildProviders(cfgs map[string]config.Provider) (map[string]provider.Provider, error) {
	providers := make(map[string]provider.Provider, len(cfgs))
	for name, c := range cfgs {
		switch c.Type {
		case "mock":
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
		default:
			// Config validation rejects unknown types, so this is a bug.
			return nil, fmt.Errorf("provider %s: unknown type %q", name, c.Type)
		}
	}
	return providers, nil
}
