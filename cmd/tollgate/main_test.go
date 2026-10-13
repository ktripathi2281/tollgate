package main

import (
	"bufio"
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

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/store"
	"github.com/ktripathi2281/tollgate/internal/store/storetest"
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

// mainCommand prepares a child process that runs main with args, with
// DATABASE_URL set to dbURL.
func mainCommand(ctx context.Context, dbURL string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), runAsMainEnv+"=1", databaseURLEnv+"="+dbURL)
	return cmd
}

// createKey stores a new key in the database at dbURL and returns it.
func createKey(t *testing.T, dbURL string) string {
	t.Helper()
	s, err := store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	secret := auth.Generate()
	_, _, err = s.CreateKey(t.Context(), store.NewKey{
		Name: "test", Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret), RPM: 1000, TPM: 1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return secret
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
		{name: "migrate help", args: []string{"migrate", "-h"}, wantStderr: "DATABASE_URL"},
		{name: "migrate extra arguments", args: []string{"migrate", "up"}, wantErr: "unexpected arguments"},
		{name: "keys without a command", args: []string{"keys"}, wantErr: "no command given", wantStderr: "create"},
		{name: "keys help", args: []string{"keys", "help"}, wantStdout: "revoke"},
		{name: "keys unknown command", args: []string{"keys", "rotate"}, wantErr: `unknown keys command "rotate"`},
		{name: "keys create without a name", args: []string{"keys", "create"}, wantErr: "-name is required"},
		{name: "keys create zero rpm", args: []string{"keys", "create", "-name", "x", "-rpm", "0"}, wantErr: "-rpm and -tpm must be at least 1"},
		{name: "keys create unknown alias", args: []string{"keys", "create", "-name", "x", "-models", "mock-fast,nope", "-config", "testdata/mock.yaml"}, wantErr: `no model alias "nope"`},
		{name: "keys create empty alias list", args: []string{"keys", "create", "-name", "x", "-models", " , ", "-config", "testdata/mock.yaml"}, wantErr: "lists no aliases"},
		{name: "keys create zero budget", args: []string{"keys", "create", "-name", "x", "-budget", "0"}, wantErr: "-budget must be a positive amount"},
		{name: "keys create budget not a number", args: []string{"keys", "create", "-name", "x", "-budget", "ten"}, wantErr: "-budget must be a positive amount"},
		{name: "keys create unknown period", args: []string{"keys", "create", "-name", "x", "-budget", "1", "-period", "weekly"}, wantErr: "-period must be daily or monthly"},
		{name: "keys create period without budget", args: []string{"keys", "create", "-name", "x", "-period", "daily"}, wantErr: "-period needs -budget"},
		{name: "reconcile extra arguments", args: []string{"reconcile", "now"}, wantErr: "unexpected arguments"},
		{name: "keys revoke without an id", args: []string{"keys", "revoke"}, wantErr: "-id is required"},
		{name: "keys list extra arguments", args: []string{"keys", "list", "all"}, wantErr: "unexpected arguments"},
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
	out, err := mainCommand(ctx, "", "serve", "--config", path).CombinedOutput()

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

// serveFails runs "tollgate serve" with a valid config and the given
// database URL, and returns its output, which must end in exit status 1.
func serveFails(t *testing.T, dbURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := mainCommand(ctx, dbURL, "serve", "--config", writeConfig(t, mockConfig)).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("got %v, want exit status 1\noutput:\n%s", err, out)
	}
	return string(out)
}

func TestServeNeedsADatabase(t *testing.T) {
	if out := serveFails(t, ""); !strings.Contains(out, "DATABASE_URL is not set") {
		t.Errorf("output does not say DATABASE_URL is missing:\n%s", out)
	}
	// Nothing listens on port 1, so the connection is refused at once.
	if out := serveFails(t, "postgres://nobody@127.0.0.1:1/none"); !strings.Contains(out, "connect to the database") {
		t.Errorf("output does not say the database is unreachable:\n%s", out)
	}
}

func TestServeRefusesAnOutOfDateSchema(t *testing.T) {
	if out := serveFails(t, storetest.EmptySchemaURL(t)); !strings.Contains(out, "run `tollgate migrate`") {
		t.Errorf("output does not say to migrate:\n%s", out)
	}
}

// Acceptance (M4), end to end: migrate, then create, list and revoke keys
// with the CLI.
func TestMigrateAndKeysCommands(t *testing.T) {
	t.Setenv(databaseURLEnv, storetest.EmptySchemaURL(t))
	ctx := t.Context()
	runOK := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := run(ctx, args, &stdout, &stderr); err != nil {
			t.Fatalf("tollgate %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
		}
		return stdout.String()
	}

	if out := runOK("migrate"); !strings.Contains(out, "applied 00001_api_keys.sql") ||
		!strings.Contains(out, "applied 00002_budgets_and_idempotency.sql") {
		t.Errorf("migrate output = %q, want both migrations applied", out)
	}
	if out := runOK("migrate"); !strings.Contains(out, "nothing to apply") {
		t.Errorf("second migrate output = %q, want nothing to apply", out)
	}

	out := runOK("keys", "create", "-name", "ci bot", "-rpm", "5", "-tpm", "500",
		"-models", "mock-fast", "-config", "testdata/mock.yaml", "-budget", "2.50", "-period", "daily")
	secret := ""
	for f := range strings.FieldsSeq(out) {
		if strings.HasPrefix(f, auth.Prefix) {
			secret = f
		}
	}
	if secret == "" {
		t.Fatalf("keys create printed no key:\n%s", out)
	}

	list := runOK("keys", "list")
	for _, want := range []string{"ci bot", auth.DisplayPrefix(secret), "mock-fast", "500", "$2.50 daily"} {
		if !strings.Contains(list, want) {
			t.Errorf("keys list does not show %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, secret) {
		t.Error("keys list shows the whole key")
	}

	id := strings.Fields(strings.Split(list, "\n")[1])[0]
	if out := runOK("keys", "revoke", "-id", id); !strings.Contains(out, "revoked") {
		t.Errorf("keys revoke output = %q", out)
	}
	// The last column is the revocation time, "-" while there is none.
	if list := runOK("keys", "list"); strings.HasSuffix(strings.TrimSpace(strings.Split(list, "\n")[1]), " -") {
		t.Errorf("keys list doesn't show the revocation:\n%s", list)
	}

	var stderr bytes.Buffer
	if err := run(ctx, []string{"keys", "revoke", "-id", "999999"}, io.Discard, &stderr); err == nil ||
		!strings.Contains(err.Error(), "there is no key 999999") {
		t.Errorf("revoking a missing key: got %v", err)
	}
}

// child is the gateway running as a separate process.
type child struct {
	cmd       *exec.Cmd
	addr      string
	adminAddr string        // "" if the admin listener is off
	logs      *json.Decoder // the rest of its JSON log, from stdout
	stderr    *bytes.Buffer
}

// startServe runs "tollgate serve" in a child process with the given config
// and database, and waits until it logs the address it is listening on.
func startServe(t *testing.T, ctx context.Context, yaml, dbURL string) *child {
	t.Helper()
	cmd := mainCommand(ctx, dbURL, "serve", "--config", writeConfig(t, yaml))
	c := &child{cmd: cmd, stderr: &bytes.Buffer{}}
	cmd.Stderr = c.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c.logs = json.NewDecoder(stdout)
	for c.addr == "" {
		var rec struct{ Msg, Addr string }
		if err := c.logs.Decode(&rec); err != nil {
			t.Fatalf("reading child logs: %v\nstderr:\n%s", err, c.stderr)
		}
		switch rec.Msg {
		case "admin listening": // logged before "listening"
			c.adminAddr = rec.Addr
		case "listening":
			c.addr = rec.Addr
		}
	}
	return c
}

// terminate sends SIGTERM, reads the rest of the log until the child
// closes stdout, and reaps it. It returns the log messages and the error
// from Wait (nil for exit status 0).
func (c *child) terminate(t *testing.T) ([]string, error) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for {
		var rec struct{ Msg string }
		err := c.logs.Decode(&rec)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading child logs: %v", err)
		}
		msgs = append(msgs, rec.Msg)
	}
	return msgs, c.cmd.Wait()
}

// Acceptance (M0): SIGTERM exits cleanly.
func TestServeExitsCleanlyOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't send SIGTERM to another process")
	}
	// The deadline only bounds a hung child; the test waits on events.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	dbURL := storetest.URL(t)
	secret := createKey(t, dbURL)
	c := startServe(t, ctx, mockConfig+"server:\n  addr: \"127.0.0.1:0\"\n  admin_addr: \"127.0.0.1:0\"\n  shutdown_grace: 5s\n", dbURL)
	checkHealthz(t, c.addr)

	// Acceptance (M4), end to end: 401 without a valid key.
	if status := getModels(t, c.addr, ""); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/models without a key: status %d, want 401", status)
	}
	if status := getModels(t, c.addr, auth.Generate()); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/models with an unknown key: status %d, want 401", status)
	}
	if status := getModels(t, c.addr, secret); status != http.StatusOK {
		t.Errorf("GET /v1/models with a key: status %d, want 200", status)
	}

	msgs, err := c.terminate(t)
	if err != nil {
		t.Fatalf("child exited with %v, want status 0\nstderr:\n%s", err, c.stderr)
	}
	for _, want := range []string{"shutting down", "shutdown complete"} {
		if !slices.Contains(msgs, want) {
			t.Errorf("logs after SIGTERM = %q, want them to include %q", msgs, want)
		}
	}
}

// Acceptance (M2), end to end: SIGTERM drains an in-flight stream. This
// runs the real binary, with a real signal and the real clock; the
// in-process tests in internal/server check the timing exactly.
func TestServeDrainsStreamOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't send SIGTERM to another process")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// 10 words, 100ms apart: the stream is still running when SIGTERM lands.
	yaml := strings.Replace(mockConfig, "output_tokens: 5", "output_tokens: 10\n    token_interval: 100ms", 1) +
		"server:\n  addr: \"127.0.0.1:0\"\n  admin_addr: \"127.0.0.1:0\"\n  shutdown_grace: 10s\n"
	dbURL := storetest.URL(t)
	secret := createKey(t, dbURL)
	c := startServe(t, ctx, yaml, dbURL)

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.addr+"/v1/chat/completions",
		strings.NewReader(`{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bufio.NewScanner(resp.Body)
	if !body.Scan() || !strings.HasPrefix(body.Text(), "data: ") {
		t.Fatalf("no first event: %q", body.Text())
	}

	// Signal the child, then read the stream to its end while it shuts down.
	done := make(chan []string, 1)
	go func() {
		var events []string
		for body.Scan() {
			if data, ok := strings.CutPrefix(body.Text(), "data: "); ok {
				events = append(events, data)
			}
		}
		done <- events
	}()
	msgs, err := c.terminate(t)
	events := <-done

	if err != nil {
		t.Errorf("child exited with %v, want status 0\nstderr:\n%s", err, c.stderr)
	}
	if len(events) == 0 || events[len(events)-1] != "[DONE]" {
		t.Errorf("the stream did not run to [DONE] during shutdown; got %q", events)
	}
	if !slices.Contains(msgs, "shutdown complete") {
		t.Errorf("logs after SIGTERM = %q, want a clean shutdown", msgs)
	}
}

// getModels sends GET /v1/models with the given key ("" sends none) and
// returns the status.
func getModels(t *testing.T, addr, key string) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
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
