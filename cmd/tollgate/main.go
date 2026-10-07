// Command tollgate runs the gateway and its admin subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `Usage: tollgate <command> [flags]

Commands:
  serve    run the gateway

Run "tollgate <command> -h" for a command's flags.
`

func main() {
	// ctx is cancelled on the first SIGINT (Ctrl-C) or SIGTERM. After that,
	// stop restores the default signal handling, so a second signal kills
	// the process at once instead of waiting for the graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tollgate:", err)
		os.Exit(1)
	}
}

// errUsage reports a missing command after the usage text has been printed.
var errUsage = errors.New("no command given")

// run is main without the process-level parts (signals, exit codes), so
// tests can call it directly.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		err = serve(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprint(stderr, usage)
		err = fmt.Errorf("unknown command %q", cmd)
	}

	// -h on a subcommand prints that command's flags and is not a failure.
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// parseFlags parses a subcommand's flags and rejects leftover arguments.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected arguments %q", fs.Name(), fs.Args())
	}
	return nil
}
