// Command mockupstream runs the mock provider as a standalone
// OpenAI-compatible HTTP server, for adapter integration tests and
// benchmarks. It shares its implementation with the in-process mock.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)

	err := run(ctx, os.Args[1:], os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockupstream:", err)
		os.Exit(1)
	}
}

// run parses flags and serves until ctx is cancelled. It reports the
// address it listens on to out.
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("mockupstream", flag.ContinueOnError)
	fs.SetOutput(out)
	addr := fs.String("addr", "127.0.0.1:9090", "address to listen on")
	var cfg mock.Config
	fs.DurationVar(&cfg.TTFT, "ttft", 100*time.Millisecond, "time to the first token")
	fs.DurationVar(&cfg.TokenInterval, "token-interval", 10*time.Millisecond, "time between tokens")
	fs.IntVar(&cfg.OutputTokens, "output-tokens", 50, "tokens per reply, or fewer if the request's limit is lower")
	fs.Float64Var(&cfg.ErrorRate, "error-rate", 0, "chance, from 0 to 1, that a request fails with 500")
	fs.IntVar(&cfg.Status, "status", 0, "fail every request with this HTTP status (400-599)")
	fs.BoolVar(&cfg.Hang, "hang", false, "never answer: wait until the client gives up")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "random seed for the generated text and failures")
	fs.IntVar(&cfg.FailAtChunk, "fail-at-chunk", 0, "streams send an error event in place of content chunk N (from 1); 0 is off")
	fs.IntVar(&cfg.StallAtChunk, "stall-at-chunk", 0, "streams stop sending at content chunk N (from 1) until the client gives up; 0 is off")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := check(cfg); err != nil {
		return err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	fmt.Fprintf(out, "listening on %s\n", ln.Addr())

	srv := &http.Server{
		Handler:           mock.Handler(mock.New("mock", cfg)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // hanging requests never finish on their own
	}
	<-errc
	return nil
}

func check(cfg mock.Config) error {
	switch {
	case cfg.OutputTokens < 1:
		return fmt.Errorf("-output-tokens must be at least 1, got %d", cfg.OutputTokens)
	case cfg.ErrorRate < 0 || cfg.ErrorRate > 1:
		return fmt.Errorf("-error-rate must be from 0 to 1, got %v", cfg.ErrorRate)
	case cfg.Status != 0 && (cfg.Status < 400 || cfg.Status > 599):
		return fmt.Errorf("-status must be from 400 to 599, got %d", cfg.Status)
	case cfg.TTFT < 0 || cfg.TokenInterval < 0:
		return errors.New("-ttft and -token-interval must not be negative")
	case cfg.FailAtChunk < 0 || cfg.StallAtChunk < 0:
		return errors.New("-fail-at-chunk and -stall-at-chunk must not be negative")
	}
	return nil
}
