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
	"net/url"
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
	Server      Server              `yaml:"server"`
	Log         Log                 `yaml:"log"`
	Timeouts    Timeouts            `yaml:"timeouts"`
	Retry       Retry               `yaml:"retry"`
	Breaker     Breaker             `yaml:"breaker"`
	Auth        Auth                `yaml:"auth"`
	Budget      Budget              `yaml:"budget"`
	Idempotency Idempotency         `yaml:"idempotency"`
	RequestLog  RequestLog          `yaml:"request_log"`
	Providers   map[string]Provider `yaml:"providers"`
	Models      map[string]Model    `yaml:"models"`
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
	// AdminAddr is the host:port of the admin listener, which serves
	// /metrics and /debug/pprof/. It is bound to localhost by default,
	// since neither should be public. Empty turns it off.
	AdminAddr string `yaml:"admin_addr"`
}

// RequestLog configures the request-log worker, which writes one row per
// chat request to the database in batches.
type RequestLog struct {
	// Buffer is how many rows can wait to be written. When it is full, new
	// rows are dropped and counted rather than slowing requests down.
	Buffer int `yaml:"buffer"`
	// BatchSize is the most rows written in one insert.
	BatchSize int `yaml:"batch_size"`
	// FlushEvery is the longest a row waits before a smaller batch is
	// written.
	FlushEvery time.Duration `yaml:"flush_every"`
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
}

// Timeouts bound how long an upstream call may take. They are per-request
// deadlines, not server-wide timeouts, so long streams aren't cut off.
type Timeouts struct {
	// Connect bounds opening a TCP connection to an upstream provider.
	Connect time.Duration `yaml:"connect"`
	// FirstToken is how long a stream may take to produce its first token,
	// from the moment the call starts.
	FirstToken time.Duration `yaml:"first_token"`
	// Idle is the longest gap allowed between stream chunks after the first
	// content. It also bounds how long one write to a slow client may block.
	Idle time.Duration `yaml:"idle"`
	// Total is the overall deadline for a request, including every attempt
	// and the whole stream.
	Total time.Duration `yaml:"total"`
}

// Retry controls retries of a failed upstream call against the same target,
// before the router falls back to the next one.
type Retry struct {
	// MaxAttemptsPerTarget is how many times a target is tried, retries
	// included.
	MaxAttemptsPerTarget int `yaml:"max_attempts_per_target"`
	// BaseBackoff and MaxBackoff shape the wait before each retry of an
	// unavailable target: exponential from BaseBackoff, capped at
	// MaxBackoff, with full jitter.
	BaseBackoff time.Duration `yaml:"base_backoff"`
	MaxBackoff  time.Duration `yaml:"max_backoff"`
	// MaxRetryAfter is the longest Retry-After from a rate-limited target
	// that the router waits out. A longer one sends it to the next target.
	MaxRetryAfter time.Duration `yaml:"max_retry_after"`
}

// Breaker configures the circuit breaker kept for each target.
type Breaker struct {
	// FailureThreshold consecutive unavailable failures open the breaker.
	FailureThreshold int `yaml:"failure_threshold"`
	// OpenFor is how long an open breaker skips its target before letting
	// one probe request through.
	OpenFor time.Duration `yaml:"open_for"`
}

// Auth configures API key checks.
type Auth struct {
	// CacheTTL is how long a key looked up in the database is remembered.
	// It is also how long a revoked key keeps working.
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// Budget configures spend budgets.
type Budget struct {
	// HoldTTL is how long a hold may stay unresolved before the sweeper
	// releases it, like an expired card authorisation. It must outlast any
	// request: timeouts.total plus server.shutdown_grace.
	HoldTTL time.Duration `yaml:"hold_ttl"`
}

// Idempotency configures Idempotency-Key handling.
type Idempotency struct {
	// LockFor is how long a request owns its key before a retry may take
	// it over as abandoned. It must be longer than timeouts.total.
	LockFor time.Duration `yaml:"lock_for"`
	// Retention is how long a completed response is kept for replay.
	Retention time.Duration `yaml:"retention"`
	// MaxResponseBytes caps the size of a stored response. A larger one
	// completes the key without being stored, and can't be replayed.
	MaxResponseBytes int `yaml:"max_response_bytes"`
}

// Provider configures one upstream provider. Type selects the
// implementation, and the remaining fields belong to that type.
type Provider struct {
	// Type is "openai" (OpenAI or any server with an OpenAI-compatible Chat
	// Completions API), "anthropic", or "mock".
	Type string `yaml:"type"`

	// BaseURL is where the API lives, such as https://api.openai.com/v1 or
	// https://api.anthropic.com. Types openai and anthropic only.
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable holding the API key. The key
	// itself never appears in the config. Types openai and anthropic only.
	APIKeyEnv string `yaml:"api_key_env"`
	// TokenParam is the request field the token limit is sent in:
	// "max_completion_tokens" (the default) or "max_tokens", because
	// OpenAI-compatible servers differ. Type openai only.
	TokenParam string `yaml:"token_param"`

	// The fields below configure type "mock", a fake provider that generates
	// text locally. TTFT is the time to the first token, TokenInterval the
	// time between tokens, and OutputTokens how many tokens a reply has
	// (fewer if the request's max tokens is lower). ErrorRate is the chance,
	// from 0 to 1, that a call fails with a 500. Status, if set, makes every
	// call fail with that HTTP status. Hang makes every call block until it
	// is cancelled. Seed makes the generated text and failures repeatable.
	// For streams, FailAtChunk (counting from 1) replaces that content chunk
	// with an error event, and StallAtChunk stops sending at that chunk
	// until the call is cancelled; 0 turns either off.
	TTFT          time.Duration `yaml:"ttft"`
	TokenInterval time.Duration `yaml:"token_interval"`
	OutputTokens  int           `yaml:"output_tokens"`
	ErrorRate     float64       `yaml:"error_rate"`
	Status        int           `yaml:"status"`
	Hang          bool          `yaml:"hang"`
	Seed          uint64        `yaml:"seed"`
	FailAtChunk   int           `yaml:"fail_at_chunk"`
	StallAtChunk  int           `yaml:"stall_at_chunk"`
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
	// Sampling says whether the model accepts temperature and top_p. Recent
	// Claude models and OpenAI reasoning models reject them. Leaving it out
	// means true.
	Sampling *bool `yaml:"sampling"`
}

// AcceptsSampling reports whether the target's model accepts temperature
// and top_p.
func (t Target) AcceptsSampling() bool {
	return t.Sampling == nil || *t.Sampling
}

// Price is a target's price in USD per million tokens. The YAML holds
// decimal strings so no float ever touches a price; Load parses them into
// the Micros fields.
type Price struct {
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
	// LongContext, if set, prices requests with long prompts: some models
	// charge more for a whole request, input and output, once its prompt
	// passes a size.
	LongContext *LongContextPrice `yaml:"long_context"`

	InputMicros  money.Micros `yaml:"-"` // micro-USD per million input tokens
	OutputMicros money.Micros `yaml:"-"` // micro-USD per million output tokens
}

// LongContextPrice replaces both of a Price's rates for a request whose
// prompt has more than AbovePromptTokens tokens.
type LongContextPrice struct {
	AbovePromptTokens int    `yaml:"above_prompt_tokens"`
	Input             string `yaml:"input"`
	Output            string `yaml:"output"`

	InputMicros  money.Micros `yaml:"-"`
	OutputMicros money.Micros `yaml:"-"`
}

// Rates returns the input and output prices, in micro-USD per million
// tokens, for a request whose prompt has promptTokens tokens. Prompt
// tokens include cached ones, as providers count them for this purpose.
func (p Price) Rates(promptTokens int) (input, output money.Micros) {
	if lc := p.LongContext; lc != nil && promptTokens > lc.AbovePromptTokens {
		return lc.InputMicros, lc.OutputMicros
	}
	return p.InputMicros, p.OutputMicros
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
			AdminAddr:         "127.0.0.1:6060",
		},
		Log: Log{Level: "info"},
		Timeouts: Timeouts{
			Connect:    5 * time.Second,
			FirstToken: 30 * time.Second,
			Idle:       30 * time.Second,
			Total:      300 * time.Second,
		},
		Retry: Retry{
			MaxAttemptsPerTarget: 2,
			BaseBackoff:          200 * time.Millisecond,
			MaxBackoff:           2 * time.Second,
			MaxRetryAfter:        2 * time.Second,
		},
		Breaker: Breaker{
			FailureThreshold: 5,
			OpenFor:          30 * time.Second,
		},
		Auth:   Auth{CacheTTL: 30 * time.Second},
		Budget: Budget{HoldTTL: 10 * time.Minute},
		Idempotency: Idempotency{
			LockFor:          6 * time.Minute,
			Retention:        24 * time.Hour,
			MaxResponseBytes: 1 << 20, // 1 MiB
		},
		RequestLog: RequestLog{Buffer: 10_000, BatchSize: 500, FlushEvery: time.Second},
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
	c.validateTimeouts(&p)
	c.validateRetry(&p)
	c.validateAuth(&p)
	c.validateMoney(&p)
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
	if s.AdminAddr != "" {
		if err := checkAddr(s.AdminAddr); err != nil {
			p.addf("server.admin_addr", "%v", err)
		} else if s.AdminAddr == s.Addr && !strings.HasSuffix(s.Addr, ":0") {
			// Port 0 asks the OS for a free port, so two of them don't clash.
			p.addf("server.admin_addr", "must differ from server.addr, got %q for both", s.Addr)
		}
	}
	l := c.RequestLog
	if l.Buffer < 1 {
		p.addf("request_log.buffer", "must be at least 1, got %d", l.Buffer)
	}
	if l.BatchSize < 1 {
		p.addf("request_log.batch_size", "must be at least 1, got %d", l.BatchSize)
	}
	if l.FlushEvery <= 0 {
		p.addf("request_log.flush_every", "must be positive, got %s", l.FlushEvery)
	}
}

func (c *Config) validateTimeouts(p *problems) {
	t := c.Timeouts
	for _, d := range []struct {
		field string
		value time.Duration
	}{
		{"timeouts.connect", t.Connect},
		{"timeouts.first_token", t.FirstToken},
		{"timeouts.idle", t.Idle},
		{"timeouts.total", t.Total},
	} {
		if d.value <= 0 {
			p.addf(d.field, "must be positive, got %s", d.value)
		}
	}
	for _, d := range []struct {
		field string
		value time.Duration
	}{
		{"timeouts.connect", t.Connect},
		{"timeouts.first_token", t.FirstToken},
		{"timeouts.idle", t.Idle},
	} {
		if d.value > t.Total {
			p.addf(d.field, "must not be longer than timeouts.total (%s), got %s", t.Total, d.value)
		}
	}
}

func (c *Config) validateRetry(p *problems) {
	r, b := c.Retry, c.Breaker
	if r.MaxAttemptsPerTarget < 1 {
		p.addf("retry.max_attempts_per_target", "must be at least 1, got %d", r.MaxAttemptsPerTarget)
	}
	if r.BaseBackoff <= 0 {
		p.addf("retry.base_backoff", "must be positive, got %s", r.BaseBackoff)
	}
	if r.MaxBackoff < r.BaseBackoff {
		p.addf("retry.max_backoff", "must be at least retry.base_backoff (%s), got %s", r.BaseBackoff, r.MaxBackoff)
	}
	if r.MaxRetryAfter < 0 {
		p.addf("retry.max_retry_after", "must not be negative, got %s", r.MaxRetryAfter)
	}
	if b.FailureThreshold < 1 {
		p.addf("breaker.failure_threshold", "must be at least 1, got %d", b.FailureThreshold)
	}
	if b.OpenFor <= 0 {
		p.addf("breaker.open_for", "must be positive, got %s", b.OpenFor)
	}
}

// maxCacheTTL caps auth.cache_ttl, because it is the revocation delay.
const maxCacheTTL = 5 * time.Minute

func (c *Config) validateAuth(p *problems) {
	if ttl := c.Auth.CacheTTL; ttl <= 0 || ttl > maxCacheTTL {
		p.addf("auth.cache_ttl", "must be positive and at most %s (it is how long a revoked key keeps working), got %s", maxCacheTTL, ttl)
	}
}

// validateMoney checks the budget and idempotency timings against the
// longest a request can run, so that the sweeper never releases the hold
// of a request that is still running, and a retry never takes over a key
// whose request is still running (DECISIONS 3).
func (c *Config) validateMoney(p *problems) {
	longest := c.Timeouts.Total + c.Server.ShutdownGrace
	if c.Budget.HoldTTL <= longest {
		p.addf("budget.hold_ttl", "must be longer than timeouts.total plus server.shutdown_grace (%s), "+
			"or the sweeper could release the hold of a running request; got %s", longest, c.Budget.HoldTTL)
	}
	i := c.Idempotency
	if i.LockFor <= c.Timeouts.Total {
		p.addf("idempotency.lock_for", "must be longer than timeouts.total (%s), "+
			"or a retry could take over a request that is still running; got %s", c.Timeouts.Total, i.LockFor)
	}
	if i.Retention < i.LockFor {
		p.addf("idempotency.retention", "must be at least idempotency.lock_for (%s), got %s", i.LockFor, i.Retention)
	}
	if i.MaxResponseBytes < 1 {
		p.addf("idempotency.max_response_bytes", "must be at least 1, got %d", i.MaxResponseBytes)
	}
}

// Provider types, and the token parameters an openai provider can send.
const (
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
	TypeMock      = "mock"

	TokenParamMaxCompletionTokens = "max_completion_tokens"
	TokenParamMaxTokens           = "max_tokens"
)

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
		case TypeOpenAI, TypeAnthropic:
			checkHTTPProvider(p, field, prov)
		case TypeMock:
			checkMock(p, field, prov)
		case "":
			p.addf(field+".type", "is required")
		default:
			p.addf(field+".type", `must be "openai", "anthropic" or "mock", got %q`, prov.Type)
		}
	}
}

// checkHTTPProvider checks a provider that is called over HTTP.
func checkHTTPProvider(p *problems, field string, prov Provider) {
	if err := checkBaseURL(prov.BaseURL); err != nil {
		p.addf(field+".base_url", "%v", err)
	}
	if prov.APIKeyEnv == "" {
		p.addf(field+".api_key_env", "is required: name the environment variable that holds the API key")
	}
	switch {
	case prov.Type == TypeAnthropic && prov.TokenParam != "":
		p.addf(field+".token_param", "applies to type openai only")
	case prov.Type == TypeOpenAI && prov.TokenParam != "" &&
		prov.TokenParam != TokenParamMaxCompletionTokens && prov.TokenParam != TokenParamMaxTokens:
		p.addf(field+".token_param", "must be %q or %q, got %q",
			TokenParamMaxCompletionTokens, TokenParamMaxTokens, prov.TokenParam)
	}
	if prov.hasMockSettings() {
		p.addf(field, "mock settings (ttft, output_tokens and so on) apply to type mock only")
	}
}

// hasMockSettings reports whether any mock-only field is set.
func (prov Provider) hasMockSettings() bool {
	return prov.TTFT != 0 || prov.TokenInterval != 0 || prov.OutputTokens != 0 || prov.ErrorRate != 0 ||
		prov.Status != 0 || prov.Hang || prov.Seed != 0 || prov.FailAtChunk != 0 || prov.StallAtChunk != 0
}

// checkBaseURL accepts an absolute http or https URL.
func checkBaseURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http or https URL, got %q", raw)
	}
	return nil
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
	if prov.FailAtChunk < 0 {
		p.addf(field+".fail_at_chunk", "must not be negative, got %d", prov.FailAtChunk)
	}
	if prov.StallAtChunk < 0 {
		p.addf(field+".stall_at_chunk", "must not be negative, got %d", prov.StallAtChunk)
	}
	if prov.BaseURL != "" || prov.APIKeyEnv != "" || prov.TokenParam != "" {
		p.addf(field, "base_url, api_key_env and token_param don't apply to type mock")
	}
}

// validatePricing parses every price into micro-USD.
func (c *Config) validatePricing(p *problems) {
	for _, key := range slices.Sorted(maps.Keys(c.Pricing)) {
		price := c.Pricing[key]
		field := fmt.Sprintf("pricing[%q]", key)
		var ok bool
		price.InputMicros, price.OutputMicros, ok = parseRates(p, field, price.Input, price.Output)
		if price.LongContext != nil {
			validateLongContext(p, field+".long_context", price, ok)
		}
		// Map values aren't addressable, so store the parsed copy back.
		c.Pricing[key] = price
	}
}

// MaxPrice caps a price: $10,000 per million tokens, about 200 times the
// most expensive model today. With it, and usage.Cost's cap on token
// counts, a cost can't overflow int64.
const MaxPrice = 10_000 * money.PerUSD

// parseRates parses an input and an output price, recording a problem for
// each one that is invalid. ok reports whether both parsed.
func parseRates(p *problems, field, input, output string) (in, out money.Micros, ok bool) {
	in, inOK := parseRate(p, field+".input", input)
	out, outOK := parseRate(p, field+".output", output)
	return in, out, inOK && outOK
}

func parseRate(p *problems, field, s string) (money.Micros, bool) {
	m, err := money.ParseUSD(s)
	if err != nil {
		p.addf(field, "%v", err)
		return 0, false
	}
	if m > MaxPrice {
		p.addf(field, "must be at most %q USD per million tokens, got %q", "10000", s)
		return 0, false
	}
	return m, true
}

// validateLongContext parses a price's long-context tier, through its
// pointer, and checks it against the base price. baseOK reports whether
// the base price parsed.
func validateLongContext(p *problems, field string, price Price, baseOK bool) {
	lc := price.LongContext
	if lc.AbovePromptTokens < 1 {
		p.addf(field+".above_prompt_tokens", "must be at least 1, got %d", lc.AbovePromptTokens)
	}
	var ok bool
	lc.InputMicros, lc.OutputMicros, ok = parseRates(p, field, lc.Input, lc.Output)
	// A budget hold prices an upper bound on the prompt, which can be over
	// the threshold when the real prompt isn't. The hold is only an upper
	// bound on the cost if crossing the threshold never lowers a price.
	if ok && baseOK && (lc.InputMicros < price.InputMicros || lc.OutputMicros < price.OutputMicros) {
		p.addf(field, "prices must not be lower than the base prices")
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
