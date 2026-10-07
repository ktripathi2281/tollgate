package config_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want config.Config
	}{
		{
			name: "empty file uses defaults",
			yaml: "",
			want: *config.Default(),
		},
		{
			name: "comments only uses defaults",
			yaml: "# nothing here\n",
			want: *config.Default(),
		},
		{
			name: "every field set",
			yaml: `
server:
  addr: "127.0.0.1:9090"
  shutdown_grace: 5s
  read_header_timeout: 2s
log:
  level: debug
`,
			want: config.Config{
				Server: config.Server{
					Addr:              "127.0.0.1:9090",
					ShutdownGrace:     5 * time.Second,
					ReadHeaderTimeout: 2 * time.Second,
				},
				Log: config.Log{Level: "debug"},
			},
		},
		{
			name: "partial file keeps the other defaults",
			yaml: "server:\n  addr: \":0\"\n",
			want: func() config.Config {
				c := *config.Default()
				c.Server.Addr = ":0"
				return c
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			if *got != tt.want {
				t.Errorf("Parse:\n got %+v\nwant %+v", *got, tt.want)
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
			name:        "unknown field, with its line number",
			yaml:        "server:\n  adress: \":8080\"\n",
			wantInError: []string{"line 2", "adress"},
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
			yaml:        "log:\n  level: info\n---\nlog:\n  level: debug\n",
			wantInError: []string{"single YAML document"},
		},
		{
			name:        "address without a port",
			yaml:        "server:\n  addr: localhost\n",
			wantInError: []string{"server.addr", "host:port"},
		},
		{
			name:        "named port",
			yaml:        "server:\n  addr: \":http\"\n",
			wantInError: []string{"server.addr", "port must be a number"},
		},
		{
			name:        "port out of range",
			yaml:        "server:\n  addr: \":70000\"\n",
			wantInError: []string{"server.addr", "70000"},
		},
		{
			name:        "empty address",
			yaml:        "server:\n  addr: \"\"\n",
			wantInError: []string{"server.addr", "must not be empty"},
		},
		{
			name:        "zero shutdown grace",
			yaml:        "server:\n  shutdown_grace: 0s\n",
			wantInError: []string{"server.shutdown_grace", "must be positive"},
		},
		{
			name:        "negative read header timeout",
			yaml:        "server:\n  read_header_timeout: -1s\n",
			wantInError: []string{"server.read_header_timeout", "must be positive"},
		},
		{
			name:        "unknown log level",
			yaml:        "log:\n  level: verbose\n",
			wantInError: []string{"log.level", `"verbose"`},
		},
		{
			name:        "log level is case-sensitive",
			yaml:        "log:\n  level: INFO\n",
			wantInError: []string{"log.level"},
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
	yaml := `
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
		if err := os.WriteFile(path, []byte("log:\n  level: warn\n"), 0o600); err != nil {
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
