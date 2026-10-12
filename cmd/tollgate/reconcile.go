package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"
)

// reconcile checks that every budget period agrees with its holds
// (invariants 1 and 2 in brief section 9.1): held equals the sum of the
// holds still held, and spent equals the sum of the settled holds' costs.
// It exits non-zero on any mismatch.
func reconcile(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: tollgate reconcile\n\nChecks every budget period in $%s against its holds.\n", databaseURLEnv)
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.CheckSchema(ctx); err != nil {
		return err
	}

	checks, err := s.ReconcileBudgets(ctx)
	if err != nil {
		return err
	}
	bad := 0
	for _, c := range checks {
		if c.OK() {
			continue
		}
		bad++
		fmt.Fprintf(stdout, "MISMATCH key %d, period %s: held %s, holds held %s; spent %s, holds settled %s\n",
			c.KeyID, c.PeriodStart.UTC().Format(time.DateOnly), c.Held, c.HoldsHeld, c.Spent, c.HoldsSettled)
	}
	fmt.Fprintf(stdout, "checked %d budget periods: %d mismatched\n", len(checks), bad)
	if bad > 0 {
		return fmt.Errorf("reconcile: %d budget periods don't match their holds", bad)
	}
	return nil
}
