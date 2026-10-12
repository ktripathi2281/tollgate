package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/limiter"
	"github.com/ktripathi2281/tollgate/internal/router"
	"github.com/ktripathi2281/tollgate/internal/server"
)

// serve runs the gateway until ctx is cancelled.
func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to the YAML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := newLogger(stdout, cfg.Log.SlogLevel())

	providers, err := buildProviders(cfg, os.Getenv, logger)
	if err != nil {
		return err
	}
	r, err := router.New(cfg, providers)
	if err != nil {
		return err
	}

	// The database holds the API keys. The gateway won't start against a
	// schema its queries don't match.
	db, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.CheckSchema(ctx); err != nil {
		return err
	}

	srv := server.New(cfg.Server, cfg.Timeouts, server.Deps{
		Router:  r,
		Keys:    auth.NewCache(db, cfg.Auth.CacheTTL, time.Now),
		Limiter: limiter.New(time.Now),
		Log:     logger,
	})

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Addr, err)
	}
	logger.Info("listening", "addr", ln.Addr().String())

	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

// newLogger returns a JSON logger. Logs go to stdout, where a container
// runtime collects them. Errors that stop the process before the logger
// exists go to stderr as plain text.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
