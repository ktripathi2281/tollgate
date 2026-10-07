package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// runAsMainEnv makes the test binary behave as the tollgate command.
const runAsMainEnv = "TEST_RUN_AS_MAIN"

func TestMain(m *testing.M) {
	// Some tests start this test binary again as a child process with
	// runAsMainEnv set. The child runs the real main, with real signal
	// handling and exit codes, instead of the tests. This avoids building a
	// separate binary for end-to-end tests.
	if os.Getenv(runAsMainEnv) == "1" {
		main()
		os.Exit(0)
	}
	goleak.VerifyTestMain(m)
}

// mainCommand prepares a child process that runs main with args.
func mainCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), runAsMainEnv+"=1")
	return cmd
}

// mockConfig is a valid config with one mock-backed alias, "mock-fast".
const mockConfig = `
providers:
  mock:
    type: mock
    output_tokens: 5
models:
  mock-fast:
    default_max_tokens: 64
    max_tokens_ceiling: 256
    targets:
      - { provider: mock, model: mock-1 }
pricing:
  mock/mock-1: { input: "1.00", output: "2.00" }
`

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    string // empty means success
		wantStdout string
		wantStderr string
	}{
		{name: "no command", args: nil, wantErr: "no command given", wantStderr: "Usage:"},
		{name: "help", args: []string{"help"}, wantStdout: "Usage:"},
		{name: "unknown command", args: []string{"launch"}, wantErr: `unknown command "launch"`, wantStderr: "Usage:"},
		{name: "serve help", args: []string{"serve", "-h"}, wantStderr: "-config"},
		{name: "serve unknown flag", args: []string{"serve", "--port", "1"}, wantErr: "flag provided but not defined"},
		{name: "serve extra arguments", args: []string{"serve", "now"}, wantErr: "unexpected arguments"},
		{name: "serve missing config", args: []string{"serve", "--config", "does-not-exist.yaml"}, wantErr: "does-not-exist.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), tt.args, &stdout, &stderr)

			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("run: unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("run: got error %v, want one containing %q", err, tt.wantErr)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// Acceptance (M0): invalid config fails at startup with a clear message.
func TestServeFailsOnInvalidConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	path := writeConfig(t, "server:\n  addr: localhost\nlog:\n  level: loud\n")
	out, err := mainCommand(ctx, "serve", "--config", path).CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("got %v, want exit status 1\noutput:\n%s", err, out)
	}
	for _, want := range []string{"invalid config", path, "server.addr", "log.level"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// Acceptance (M0): SIGTERM exits cleanly.
func TestServeExitsCleanlyOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't send SIGTERM to another process")
	}
	// The deadline only bounds a hung child; the test waits on events.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	path := writeConfig(t, mockConfig+"server:\n  addr: \"127.0.0.1:0\"\n  shutdown_grace: 5s\n")
	cmd := mainCommand(ctx, "serve", "--config", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The child logs JSON lines. Read until it says where it's listening.
	logs := json.NewDecoder(stdout)
	var addr string
	for addr == "" {
		var rec struct{ Msg, Addr string }
		if err := logs.Decode(&rec); err != nil {
			t.Fatalf("reading child logs: %v\nstderr:\n%s", err, stderr.String())
		}
		if rec.Msg == "listening" {
			addr = rec.Addr
		}
	}

	checkHealthz(t, addr)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	// Read the remaining logs until the child closes stdout, then reap it.
	var msgs []string
	for {
		var rec struct{ Msg string }
		err := logs.Decode(&rec)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading child logs: %v", err)
		}
		msgs = append(msgs, rec.Msg)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exited with %v, want status 0\nstderr:\n%s", err, stderr.String())
	}
	for _, want := range []string{"shutting down", "shutdown complete"} {
		if !slices.Contains(msgs, want) {
			t.Errorf("logs after SIGTERM = %q, want them to include %q", msgs, want)
		}
	}
}

func checkHealthz(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", resp.StatusCode)
	}
}
