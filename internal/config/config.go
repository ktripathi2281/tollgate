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
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the whole gateway configuration. Each milestone adds the sections
// it needs.
type Config struct {
	Server Server `yaml:"server"`
	Log    Log    `yaml:"log"`
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
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
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
func Default() *Config {
	return &Config{
		Server: Server{
			Addr:              ":8080",
			ShutdownGrace:     30 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
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

func (c *Config) validate() error {
	var p problems

	if err := checkAddr(c.Server.Addr); err != nil {
		p.addf("server.addr", "%v", err)
	}
	if c.Server.ShutdownGrace <= 0 {
		p.addf("server.shutdown_grace", "must be positive, got %s", c.Server.ShutdownGrace)
	}
	if c.Server.ReadHeaderTimeout <= 0 {
		p.addf("server.read_header_timeout", "must be positive, got %s", c.Server.ReadHeaderTimeout)
	}
	if _, ok := logLevels[c.Log.Level]; !ok {
		p.addf("log.level", "must be one of debug, info, warn, error; got %q", c.Log.Level)
	}

	if len(p) > 0 {
		return &ValidationError{Problems: p}
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
