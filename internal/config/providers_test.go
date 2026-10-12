package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
)

// realProviders configures one provider of each HTTP type and an alias
// that uses both, the second without sampling.
const realProviders = `
providers:
  openai:
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
  local:
    type: openai
    base_url: http://localhost:11434/v1
    api_key_env: LOCAL_KEY
    token_param: max_tokens
  anthropic:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
models:
  fast:
    default_max_tokens: 100
    max_tokens_ceiling: 1000
    targets:
      - { provider: openai, model: gpt-x }
      - { provider: anthropic, model: claude-x, sampling: false }
pricing:
  openai/gpt-x: { input: "0.15", output: "0.60" }
  anthropic/claude-x: { input: "1", output: "5" }
`

func TestParseHTTPProviders(t *testing.T) {
	cfg, err := config.Parse([]byte(realProviders))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]config.Provider{
		"openai":    {Type: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		"local":     {Type: "openai", BaseURL: "http://localhost:11434/v1", APIKeyEnv: "LOCAL_KEY", TokenParam: "max_tokens"},
		"anthropic": {Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"},
	}
	if !reflect.DeepEqual(cfg.Providers, want) {
		t.Errorf("Providers = %+v\nwant %+v", cfg.Providers, want)
	}
	targets := cfg.Models["fast"].Targets
	if !targets[0].AcceptsSampling() || targets[1].AcceptsSampling() {
		t.Errorf("AcceptsSampling = %v, %v; want true for the default and false when sampling: false",
			targets[0].AcceptsSampling(), targets[1].AcceptsSampling())
	}
}

func TestParseRetryAndBreaker(t *testing.T) {
	cfg, err := config.Parse([]byte(minimal + `
retry: { max_attempts_per_target: 3, base_backoff: 100ms, max_backoff: 1s, max_retry_after: 0s }
breaker: { failure_threshold: 2, open_for: 10s }
timeouts: { connect: 2s }
auth: { cache_ttl: 10s }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Auth.CacheTTL != 10*time.Second {
		t.Errorf("Auth.CacheTTL = %v, want 10s", cfg.Auth.CacheTTL)
	}
	if def := config.Default().Auth.CacheTTL; def != 30*time.Second {
		t.Errorf("default Auth.CacheTTL = %v, want 30s", def)
	}
	if want := (config.Retry{MaxAttemptsPerTarget: 3, BaseBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}); cfg.Retry != want {
		t.Errorf("Retry = %+v, want %+v", cfg.Retry, want)
	}
	if want := (config.Breaker{FailureThreshold: 2, OpenFor: 10 * time.Second}); cfg.Breaker != want {
		t.Errorf("Breaker = %+v, want %+v", cfg.Breaker, want)
	}
	if cfg.Timeouts.Connect != 2*time.Second || cfg.Timeouts.Total != 300*time.Second {
		t.Errorf("Timeouts = %+v, want connect 2s and the other defaults", cfg.Timeouts)
	}
}

func TestParseRejectsProviderMistakes(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantInError []string
	}{
		{
			name:        "missing base URL and key variable",
			yaml:        strings.Replace(strings.Replace(realProviders, "    base_url: https://api.openai.com/v1\n", "", 1), "    api_key_env: OPENAI_API_KEY\n", "", 1),
			wantInError: []string{"providers.openai.base_url: is required", "providers.openai.api_key_env: is required"},
		},
		{
			name:        "relative base URL",
			yaml:        strings.Replace(realProviders, "https://api.anthropic.com", "api.anthropic.com", 1),
			wantInError: []string{"providers.anthropic.base_url", "absolute http or https URL"},
		},
		{
			name:        "unknown token parameter",
			yaml:        strings.Replace(realProviders, "token_param: max_tokens", "token_param: max_output_tokens", 1),
			wantInError: []string{"providers.local.token_param", `got "max_output_tokens"`},
		},
		{
			name:        "token parameter on anthropic",
			yaml:        strings.Replace(realProviders, "    api_key_env: ANTHROPIC_API_KEY\n", "    api_key_env: ANTHROPIC_API_KEY\n    token_param: max_tokens\n", 1),
			wantInError: []string{"providers.anthropic.token_param: applies to type openai only"},
		},
		{
			name:        "mock settings on a real provider",
			yaml:        strings.Replace(realProviders, "    api_key_env: OPENAI_API_KEY\n", "    api_key_env: OPENAI_API_KEY\n    output_tokens: 5\n", 1),
			wantInError: []string{"providers.openai: mock settings"},
		},
		{
			name:        "real-provider settings on a mock",
			yaml:        strings.Replace(minimal, "    output_tokens: 10\n", "    output_tokens: 10\n    base_url: http://x\n", 1),
			wantInError: []string{"providers.mock: base_url, api_key_env and token_param don't apply to type mock"},
		},
		{
			name: "bad retry and breaker",
			yaml: minimal + "retry: { max_attempts_per_target: 0, base_backoff: 0s, max_backoff: -1s, max_retry_after: -1s }\n" +
				"breaker: { failure_threshold: 0, open_for: 0s }\n",
			wantInError: []string{
				"retry.max_attempts_per_target", "retry.base_backoff", "retry.max_backoff", "retry.max_retry_after",
				"breaker.failure_threshold", "breaker.open_for",
			},
		},
		{
			name:        "connect timeout",
			yaml:        minimal + "timeouts: { connect: 10m }\n",
			wantInError: []string{"timeouts.connect: must not be longer than timeouts.total"},
		},
		{
			name:        "zero key cache TTL",
			yaml:        minimal + "auth: { cache_ttl: 0s }\n",
			wantInError: []string{"auth.cache_ttl: must be positive and at most 5m0s", "got 0s"},
		},
		{
			name:        "key cache TTL too long to be a revocation delay",
			yaml:        minimal + "auth: { cache_ttl: 1h }\n",
			wantInError: []string{"auth.cache_ttl", "got 1h0m0s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse: expected an error, got nil")
			}
			for _, want := range tt.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
