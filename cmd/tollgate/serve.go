package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/budget"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/idempotency"
	"github.com/ktripathi2281/tollgate/internal/limiter"
	"github.com/ktripathi2281/tollgate/internal/metrics"
	"github.com/ktripathi2281/tollgate/internal/router"
	"github.com/ktripathi2281/tollgate/internal/server"
	"github.com/ktripathi2281/tollgate/internal/usage"
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

	budgets := budget.New(db, cfg.Budget.HoldTTL, time.Now, logger)
	idem := idempotency.New(db, cfg.Idempotency.LockFor, cfg.Idempotency.Retention,
		cfg.Idempotency.MaxResponseBytes, time.Now, logger)
	requests := usage.NewRecorder(db, cfg.RequestLog.Buffer, cfg.RequestLog.BatchSize, cfg.RequestLog.FlushEvery, logger)
	m := metrics.New()
	exportStats(m, requests, budgets)
	srv := server.New(cfg.Server, cfg.Timeouts, server.Deps{
		Router:      r,
		Keys:        auth.NewCache(db, cfg.Auth.CacheTTL, time.Now),
		Limiter:     limiter.New(time.Now),
		Budgets:     budgets,
		Idempotency: idem,
		Pricing:     cfg.Pricing,
		Metrics:     m,
		RequestLog:  requests,
		DB:          db,
		Log:         logger,
	})

	// Background work: the sweeper, the request-log worker and the admin
	// listener. It runs until the server has finished with requests. Then
	// the log worker writes what is still queued, and serve waits for all
	// of it before the deferred db.Close runs.
	bgCtx, cancelBackground := context.WithCancel(context.Background())
	var background sync.WaitGroup
	stopBackground := func() {
		cancelBackground()
		background.Wait()
	}
	defer stopBackground() // for the early returns; a second call does nothing
	background.Go(func() {
		sweepEvery(bgCtx, sweepInterval, func(ctx context.Context) { sweepOnce(ctx, budgets, idem, logger) })
	})
	background.Go(func() { requests.Run(bgCtx) })

	var lc net.ListenConfig
	if cfg.Server.AdminAddr != "" {
		aln, err := lc.Listen(ctx, "tcp", cfg.Server.AdminAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", cfg.Server.AdminAddr, err)
		}
		logger.Info("admin listening", "addr", aln.Addr().String())
		background.Go(func() {
			if err := server.ServeAdmin(bgCtx, aln, server.AdminHandler(m)); err != nil {
				logger.Error("admin listener failed", "error", err)
			}
		})
	}
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Addr, err)
	}
	logger.Info("listening", "addr", ln.Addr().String())

	err = srv.Serve(ctx, ln)
	stopBackground()
	if err != nil {
		return err
	}
	logger.Info("shutdown complete",
		"request_logs_written", requests.Written.Load(), "request_logs_dropped", requests.Dropped.Load())
	return nil
}

// exportStats exports, as metrics, the counters that the request log and
// the budgets keep themselves.
func exportStats(m *metrics.Metrics, r *usage.Recorder, b *budget.Budgets) {
	count := func(c *atomic.Int64) func() float64 {
		return func() float64 { return float64(c.Load()) }
	}
	m.Counter("request_logs_dropped_total", "Request-log rows dropped because the queue was full.", count(&r.Dropped))
	m.Counter("request_logs_written_total", "Request-log rows written.", count(&r.Written))
	m.Counter("request_logs_failed_total", "Request-log rows lost because a write failed.", count(&r.Failed))
	m.Gauge("request_logs_queued", "Request-log rows queued for the worker, not counting the batch it is filling.",
		func() float64 { return float64(r.Queued()) })
	m.Counter("budget_settle_failures_total", "Settles that failed after every retry; their cost is not recorded.", count(&b.Stats.SettleFailures))
	m.Counter("budget_late_settles_total", "Settles that found the sweeper had released the hold; their cost is not recorded.", count(&b.Stats.LateSettles))
	m.Counter("budget_holds_swept_total", "Expired holds released by the sweeper.", count(&b.Stats.Swept))
}

// newLogger returns a JSON logger. Logs go to stdout, where a container
// runtime collects them. Errors that stop the process before the logger
// exists go to stderr as plain text.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
