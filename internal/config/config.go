// Package config loads and validates the gateway's YAML configuration.
//
// Secrets never live in the config file. Fields such as a provider's
// api_key_env name an environment variable, and the component that needs the
// secret reads it from the environment.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/ktripathi2281/tollgate/internal/money"
)

// Config is the whole gateway configuration. Each milestone adds the sections
// it needs.
type Config struct {
	Server    Server              `yaml:"server"`
	Log       Log                 `yaml:"log"`
	Providers map[string]Provider `yaml:"providers"`
	Models    map[string]Model    `yaml:"models"`
	// Pricing is keyed by "provider/model"; see PriceKey.
	Pricing map[string]Price `yaml:"pricing"`
}

// Server configures the public HTTP listener.
type Server struct {
	// Addr is the host:port to listen on, for example ":8080".
	Addr string `yaml:"addr"`
	// ShutdownGrace is how long in-flight requests may run after a shutdown
	// signal before they are cancelled.
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers, which protects against slow-header attacks.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// MaxBodyBytes caps the size of a request body.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxInflight caps how many API requests are processed at once. Requests
	// over the cap are shed with 503 instead of queueing.
	MaxInflight int `yaml:"max_inflight"`
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
}

// Provider configures one upstream provider. Type selects the
// implementation, and the remaining fields belong to that type.
type Provider struct {
	// Type is the implementation: "mock" for now.
	Type string `yaml:"type"`

	// The fields below configure type "mock", a fake provider that generates
	// text locally. TTFT is the time to the first token, TokenInterval the
	// time between tokens, and OutputTokens how many tokens a reply has
	// (fewer if the request's max tokens is lower). ErrorRate is the chance,
	// from 0 to 1, that a call fails with a 500. Status, if set, makes every
	// call fail with that HTTP status. Hang makes every call block until it
	// is cancelled. Seed makes the generated text and failures repeatable.
	TTFT          time.Duration `yaml:"ttft"`
	TokenInterval time.Duration `yaml:"token_interval"`
	OutputTokens  int           `yaml:"output_tokens"`
	ErrorRate     float64       `yaml:"error_rate"`
	Status        int           `yaml:"status"`
	Hang          bool          `yaml:"hang"`
	Seed          uint64        `yaml:"seed"`
}

// Model is a model alias: the name clients send as "model", mapped to an
// ordered list of upstream targets.
type Model struct {
	// DefaultMaxTokens is sent upstream when the client sets no token limit,
	// so every request has a real upper bound on output.
	DefaultMaxTokens int `yaml:"default_max_tokens"`
	// MaxTokensCeiling is the largest token limit a client may ask for.
	MaxTokensCeiling int `yaml:"max_tokens_ceiling"`
	// Targets are tried in order.
	Targets []Target `yaml:"targets"`
}

// Target is one upstream provider and the model ID to request from it.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Price is a target's price in USD per million tokens. The YAML holds
// decimal strings so no float ever touches a price; Load parses them into
// the Micros fields.
type Price struct {
	Input  string `yaml:"input"`
	Output string `yaml:"output"`

	InputMicros  money.Micros `yaml:"-"` // micro-USD per million input tokens
	OutputMicros money.Micros `yaml:"-"` // micro-USD per million output tokens
}

// PriceKey returns the Pricing key for a target: "provider/model".
func PriceKey(provider, model string) string {
	return provider + "/" + model
}

// logLevels maps the accepted level names to slog levels.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// SlogLevel returns the configured level. Load has already rejected unknown
// names, so the fallback to info is never used on a loaded config.
func (l Log) SlogLevel() slog.Level {
	if level, ok := logLevels[l.Level]; ok {
		return level
	}
	return slog.LevelInfo
}

// Default returns the configuration used for every field a file leaves out.
// Providers, models and pricing have no defaults: a file must define them.
func Default() *Config {
	return &Config{
		Server: Server{
			Addr:              ":8080",
			ShutdownGrace:     30 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
			MaxBodyBytes:      2 << 20, // 2 MiB
			MaxInflight:       2000,
		},
		Log: Log{Level: "info"},
	}
}

// Load reads the YAML file at path, applies defaults for the fields it leaves
// out, and validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes YAML into a Config on top of Default and validates it.
// Unknown fields are errors, so a misspelt key fails loudly instead of being
// silently ignored.
func Parse(data []byte) (*Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// io.EOF means the file is empty, which leaves every default in place.
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	// A second document would be silently ignored, so reject it.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse: the file must contain a single YAML document")
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ValidationError lists every problem found in a config, so a broken file
// can be fixed in one pass rather than one error per restart.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// problems collects validation failures as "field: message" lines.
type problems []string

func (p *problems) addf(field, format string, args ...any) {
	*p = append(*p, field+": "+fmt.Sprintf(format, args...))
}

// validate checks every section and parses prices. Map keys are visited in
// sorted order so the error list is the same on every run.
func (c *Config) validate() error {
	var p problems
	c.validateServer(&p)
	if _, ok := logLevels[c.Log.Level]; !ok {
		p.addf("log.level", "must be one of debug, info, warn, error; got %q", c.Log.Level)
	}
	c.validateProviders(&p)
	c.validatePricing(&p)
	c.validateModels(&p)

	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func (c *Config) validateServer(p *problems) {
	s := c.Server
	if err := checkAddr(s.Addr); err != nil {
		p.addf("server.addr", "%v", err)
	}
	if s.ShutdownGrace <= 0 {
		p.addf("server.shutdown_grace", "must be positive, got %s", s.ShutdownGrace)
	}
	if s.ReadHeaderTimeout <= 0 {
		p.addf("server.read_header_timeout", "must be positive, got %s", s.ReadHeaderTimeout)
	}
	if s.MaxBodyBytes <= 0 {
		p.addf("server.max_body_bytes", "must be positive, got %d", s.MaxBodyBytes)
	}
	if s.MaxInflight <= 0 {
		p.addf("server.max_inflight", "must be positive, got %d", s.MaxInflight)
	}
}

func (c *Config) validateProviders(p *problems) {
	if len(c.Providers) == 0 {
		p.addf("providers", "at least one provider is required")
	}
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		prov := c.Providers[name]
		field := "providers." + name
		if err := checkName(name); err != nil {
			p.addf(field, "%v", err)
		}
		switch prov.Type {
		case "mock":
			checkMock(p, field, prov)
		case "":
			p.addf(field+".type", "is required")
		default:
			p.addf(field+".type", `must be "mock", got %q`, prov.Type)
		}
	}
}

func checkMock(p *problems, field string, prov Provider) {
	if prov.TTFT < 0 {
		p.addf(field+".ttft", "must not be negative, got %s", prov.TTFT)
	}
	if prov.TokenInterval < 0 {
		p.addf(field+".token_interval", "must not be negative, got %s", prov.TokenInterval)
	}
	if prov.OutputTokens < 1 {
		p.addf(field+".output_tokens", "must be at least 1, got %d", prov.OutputTokens)
	}
	if prov.ErrorRate < 0 || prov.ErrorRate > 1 {
		p.addf(field+".error_rate", "must be from 0 to 1, got %v", prov.ErrorRate)
	}
	if prov.Status != 0 && (prov.Status < 400 || prov.Status > 599) {
		p.addf(field+".status", "must be an HTTP error status from 400 to 599, got %d", prov.Status)
	}
}

// validatePricing parses every price into micro-USD.
func (c *Config) validatePricing(p *problems) {
	for _, key := range slices.Sorted(maps.Keys(c.Pricing)) {
		price := c.Pricing[key]
		field := fmt.Sprintf("pricing[%q]", key)
		var err error
		if price.InputMicros, err = money.ParseUSD(price.Input); err != nil {
			p.addf(field+".input", "%v", err)
		}
		if price.OutputMicros, err = money.ParseUSD(price.Output); err != nil {
			p.addf(field+".output", "%v", err)
		}
		// Map values aren't addressable, so store the parsed copy back.
		c.Pricing[key] = price
	}
}

func (c *Config) validateModels(p *problems) {
	if len(c.Models) == 0 {
		p.addf("models", "at least one model alias is required")
	}
	for _, alias := range slices.Sorted(maps.Keys(c.Models)) {
		m := c.Models[alias]
		field := "models." + alias
		if alias == "" || strings.ContainsFunc(alias, unicode.IsSpace) {
			p.addf(field, "alias must be non-empty and contain no whitespace, got %q", alias)
		}
		if m.DefaultMaxTokens < 1 {
			p.addf(field+".default_max_tokens", "must be at least 1, got %d", m.DefaultMaxTokens)
		}
		if m.MaxTokensCeiling < m.DefaultMaxTokens {
			p.addf(field+".max_tokens_ceiling", "must be at least default_max_tokens (%d), got %d",
				m.DefaultMaxTokens, m.MaxTokensCeiling)
		}
		if len(m.Targets) == 0 {
			p.addf(field+".targets", "at least one target is required")
		}
		for i, t := range m.Targets {
			tfield := fmt.Sprintf("%s.targets[%d]", field, i)
			if _, ok := c.Providers[t.Provider]; !ok {
				p.addf(tfield+".provider", "unknown provider %q", t.Provider)
			}
			if t.Model == "" {
				p.addf(tfield+".model", "is required")
			}
			// Every target needs a price so spend can always be computed.
			if _, ok := c.Pricing[PriceKey(t.Provider, t.Model)]; !ok {
				p.addf(tfield, "no price for %q in pricing", PriceKey(t.Provider, t.Model))
			}
		}
	}
}

// checkName accepts provider names that can be used in "provider/model"
// pricing keys.
func checkName(name string) error {
	if name == "" || strings.ContainsFunc(name, unicode.IsSpace) || strings.Contains(name, "/") {
		return fmt.Errorf("name must be non-empty with no whitespace or '/', got %q", name)
	}
	return nil
}

// checkAddr accepts host:port with a numeric port. An empty host means all
// interfaces, and port 0 asks the OS for a free port (used in tests).
func checkAddr(addr string) error {
	if addr == "" {
		return errors.New("must not be empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port, got %q", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("port must be a number from 0 to 65535, got %q", port)
	}
	return nil
}
