package config_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/money"
)

// minimal is the smallest valid config: one provider, one alias, its price.
const minimal = `
providers:
  mock:
    type: mock
    output_tokens: 10
models:
  mock-fast:
    default_max_tokens: 256
    max_tokens_ceiling: 1024
    targets:
      - { provider: mock, model: mock-1 }
pricing:
  mock/mock-1: { input: "1.00", output: "2.00" }
`

// minimalConfig is what minimal parses to.
func minimalConfig() config.Config {
	c := *config.Default()
	c.Providers = map[string]config.Provider{
		"mock": {Type: "mock", OutputTokens: 10},
	}
	c.Models = map[string]config.Model{
		"mock-fast": {
			DefaultMaxTokens: 256,
			MaxTokensCeiling: 1024,
			Targets:          []config.Target{{Provider: "mock", Model: "mock-1"}},
		},
	}
	c.Pricing = map[string]config.Price{
		"mock/mock-1": {
			Input: "1.00", Output: "2.00",
			InputMicros: 1 * money.PerUSD, OutputMicros: 2 * money.PerUSD,
		},
	}
	return c
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want func() config.Config
	}{
		{
			name: "minimal file uses defaults for everything else",
			yaml: minimal,
			want: minimalConfig,
		},
		{
			name: "server and log fields set",
			yaml: minimal + `
server:
  addr: "127.0.0.1:9090"
  shutdown_grace: 5s
  read_header_timeout: 2s
  max_body_bytes: 1024
  max_inflight: 8
log:
  level: debug
`,
			want: func() config.Config {
				c := minimalConfig()
				c.Server = config.Server{
					Addr:              "127.0.0.1:9090",
					ShutdownGrace:     5 * time.Second,
					ReadHeaderTimeout: 2 * time.Second,
					MaxBodyBytes:      1024,
					MaxInflight:       8,
				}
				c.Log = config.Log{Level: "debug"}
				return c
			},
		},
		{
			name: "every mock field set",
			yaml: `
providers:
  flaky:
    type: mock
    ttft: 100ms
    token_interval: 10ms
    output_tokens: 50
    error_rate: 0.25
    status: 429
    hang: true
    seed: 42
models:
  m:
    default_max_tokens: 1
    max_tokens_ceiling: 1
    targets: [{ provider: flaky, model: x }]
pricing:
  flaky/x: { input: "0.075", output: "0" }
`,
			want: func() config.Config {
				c := *config.Default()
				c.Providers = map[string]config.Provider{"flaky": {
					Type: "mock", TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond,
					OutputTokens: 50, ErrorRate: 0.25, Status: 429, Hang: true, Seed: 42,
				}}
				c.Models = map[string]config.Model{"m": {
					DefaultMaxTokens: 1, MaxTokensCeiling: 1,
					Targets: []config.Target{{Provider: "flaky", Model: "x"}},
				}}
				c.Pricing = map[string]config.Price{"flaky/x": {
					Input: "0.075", Output: "0", InputMicros: 75_000, OutputMicros: 0,
				}}
				return c
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			if want := tt.want(); !reflect.DeepEqual(*got, want) {
				t.Errorf("Parse:\n got %+v\nwant %+v", *got, want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		// wantInError lists substrings the error message must contain.
		wantInError []string
	}{
		{
			name:        "empty file has no providers or models",
			yaml:        "",
			wantInError: []string{"providers: at least one provider", "models: at least one model alias"},
		},
		{
			name:        "unknown field, with its line number",
			yaml:        "server:\n  port: 8080\n",
			wantInError: []string{"line 2", "port"},
		},
		{
			name:        "unknown section",
			yaml:        "servre:\n  addr: \":8080\"\n",
			wantInError: []string{"servre"},
		},
		{
			name:        "duration without a unit",
			yaml:        "server:\n  shutdown_grace: 30\n",
			wantInError: []string{"line 2"},
		},
		{
			name:        "malformed YAML",
			yaml:        "server: [\n",
			wantInError: []string{"parse"},
		},
		{
			name:        "two documents",
			yaml:        minimal + "---\nlog:\n  level: debug\n",
			wantInError: []string{"single YAML document"},
		},
		{
			name:        "address without a port",
			yaml:        minimal + "server:\n  addr: localhost\n",
			wantInError: []string{"server.addr", "host:port"},
		},
		{
			name:        "named port",
			yaml:        minimal + "server:\n  addr: \":http\"\n",
			wantInError: []string{"server.addr", "port must be a number"},
		},
		{
			name:        "port out of range",
			yaml:        minimal + "server:\n  addr: \":70000\"\n",
			wantInError: []string{"server.addr", "70000"},
		},
		{
			name:        "empty address",
			yaml:        minimal + "server:\n  addr: \"\"\n",
			wantInError: []string{"server.addr", "must not be empty"},
		},
		{
			name:        "zero shutdown grace",
			yaml:        minimal + "server:\n  shutdown_grace: 0s\n",
			wantInError: []string{"server.shutdown_grace", "must be positive"},
		},
		{
			name:        "negative read header timeout",
			yaml:        minimal + "server:\n  read_header_timeout: -1s\n",
			wantInError: []string{"server.read_header_timeout", "must be positive"},
		},
		{
			name:        "zero body limit",
			yaml:        minimal + "server:\n  max_body_bytes: 0\n",
			wantInError: []string{"server.max_body_bytes", "must be positive"},
		},
		{
			name:        "zero in-flight cap",
			yaml:        minimal + "server:\n  max_inflight: 0\n",
			wantInError: []string{"server.max_inflight", "must be positive"},
		},
		{
			name:        "unknown log level",
			yaml:        minimal + "log:\n  level: verbose\n",
			wantInError: []string{"log.level", `"verbose"`},
		},
		{
			name:        "log level is case-sensitive",
			yaml:        minimal + "log:\n  level: INFO\n",
			wantInError: []string{"log.level"},
		},
		{
			name: "unknown provider type",
			yaml: strings.Replace(minimal, "type: mock", "type: openai", 1),
			// The model's target still points at the provider, so only the
			// type is reported.
			wantInError: []string{"providers.mock.type", `got "openai"`},
		},
		{
			name:        "missing provider type",
			yaml:        strings.Replace(minimal, "type: mock", "type: \"\"", 1),
			wantInError: []string{"providers.mock.type: is required"},
		},
		{
			name:        "provider name with a slash",
			yaml:        strings.Replace(minimal, "  mock:\n", "  \"mo/ck\":\n", 1),
			wantInError: []string{`providers.mo/ck`, "no whitespace or '/'"},
		},
		{
			name: "bad mock settings",
			yaml: strings.Replace(minimal, "output_tokens: 10",
				"output_tokens: 0\n    ttft: -1s\n    token_interval: -1ms\n    error_rate: 1.5\n    status: 200", 1),
			wantInError: []string{
				"providers.mock.output_tokens", "providers.mock.ttft", "providers.mock.token_interval",
				"providers.mock.error_rate", "providers.mock.status",
			},
		},
		{
			name: "alias token limits",
			yaml: strings.Replace(strings.Replace(minimal, "default_max_tokens: 256", "default_max_tokens: 0", 1),
				"max_tokens_ceiling: 1024", "max_tokens_ceiling: -1", 1),
			wantInError: []string{"models.mock-fast.default_max_tokens", "models.mock-fast.max_tokens_ceiling"},
		},
		{
			name:        "ceiling below default",
			yaml:        strings.Replace(minimal, "max_tokens_ceiling: 1024", "max_tokens_ceiling: 100", 1),
			wantInError: []string{"models.mock-fast.max_tokens_ceiling", "at least default_max_tokens (256)"},
		},
		{
			name:        "alias without targets",
			yaml:        strings.Replace(minimal, "      - { provider: mock, model: mock-1 }\n", "", 1),
			wantInError: []string{"models.mock-fast.targets: at least one target"},
		},
		{
			name:        "target with unknown provider and no price",
			yaml:        strings.Replace(minimal, "{ provider: mock, model: mock-1 }", "{ provider: nope, model: mock-1 }", 1),
			wantInError: []string{`models.mock-fast.targets[0].provider: unknown provider "nope"`, `no price for "nope/mock-1"`},
		},
		{
			name:        "target without a model",
			yaml:        strings.Replace(minimal, "model: mock-1 }", "model: \"\" }", 1),
			wantInError: []string{"models.mock-fast.targets[0].model: is required"},
		},
		{
			name:        "alias with whitespace",
			yaml:        strings.Replace(minimal, "  mock-fast:\n", "  \"mock fast\":\n", 1),
			wantInError: []string{"models.mock fast", "no whitespace"},
		},
		{
			name: "unparseable prices",
			yaml: strings.Replace(minimal, `{ input: "1.00", output: "2.00" }`,
				`{ input: "-1", output: "0.0000001" }`, 1),
			wantInError: []string{`pricing["mock/mock-1"].input`, `pricing["mock/mock-1"].output`},
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

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	yaml := minimal + `
server:
  addr: localhost
  shutdown_grace: 0s
  read_header_timeout: -1s
log:
  level: verbose
`
	_, err := config.Parse([]byte(yaml))

	var verr *config.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a *config.ValidationError, got %T: %v", err, err)
	}
	wantFields := []string{"server.addr", "server.shutdown_grace", "server.read_header_timeout", "log.level"}
	if len(verr.Problems) != len(wantFields) {
		t.Fatalf("got %d problems, want %d:\n%v", len(verr.Problems), len(wantFields), err)
	}
	for i, field := range wantFields {
		if !strings.HasPrefix(verr.Problems[i], field+": ") {
			t.Errorf("problem %d = %q, want it to start with %q", i, verr.Problems[i], field)
		}
	}
}

func TestLoad(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(minimal+"log:\n  level: warn\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Log.SlogLevel(); got != slog.LevelWarn {
			t.Errorf("SlogLevel() = %v, want %v", got, slog.LevelWarn)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Load: got %v, want an error wrapping fs.ErrNotExist", err)
		}
	})

	t.Run("invalid file names the path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(path, []byte("log:\n  level: loud\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("Load: got %v, want an error that names %s", err, path)
		}
	})
}
