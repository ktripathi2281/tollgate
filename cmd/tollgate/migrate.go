package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

// migrate applies the database migrations that haven't been applied yet.
func migrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: tollgate migrate\n\nApplies pending migrations to the database at $%s.\n", databaseURLEnv)
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	applied, version, err := s.Migrate(ctx)
	for _, name := range applied {
		fmt.Fprintln(stdout, "applied", name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "nothing to apply")
	}
	fmt.Fprintln(stdout, "schema version", version)
	return nil
}
