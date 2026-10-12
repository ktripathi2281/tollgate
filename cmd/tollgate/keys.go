package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/store"
)

const keysUsage = `Usage: tollgate keys <command> [flags]

Commands:
  create   create a key and print it, once
  list     list every key
  revoke   revoke a key

Keys live in the database at $DATABASE_URL. Run "tollgate keys <command> -h"
for a command's flags.
`

// keys manages virtual API keys.
func keys(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, keysUsage)
		return errUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "create":
		return keysCreate(ctx, rest, stdout, stderr)
	case "list":
		return keysList(ctx, rest, stdout, stderr)
	case "revoke":
		return keysRevoke(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, keysUsage)
		return nil
	default:
		fmt.Fprint(stderr, keysUsage)
		return fmt.Errorf("unknown keys command %q", cmd)
	}
}

func keysCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "a name for the key, to tell keys apart (required)")
	rpm := fs.Int("rpm", 60, "requests per minute")
	tpm := fs.Int("tpm", 100_000, "tokens per minute: estimated prompt plus max_tokens")
	models := fs.String("models", "", "comma-separated aliases the key may use (default: every alias)")
	configPath := fs.String("config", "config.yaml", "the config file, to check -models against")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	// Check everything before touching the database.
	if strings.TrimSpace(*name) == "" {
		return errors.New("keys create: -name is required")
	}
	if *rpm < 1 || *tpm < 1 {
		return fmt.Errorf("keys create: -rpm and -tpm must be at least 1, got %d and %d", *rpm, *tpm)
	}
	allowed, err := parseModels(*models, *configPath)
	if err != nil {
		return err
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	secret := auth.Generate()
	id, _, err := s.CreateKey(ctx, store.NewKey{
		Name:          strings.TrimSpace(*name),
		Hash:          auth.Hash(secret),
		Prefix:        auth.DisplayPrefix(secret),
		RPM:           *rpm,
		TPM:           *tpm,
		AllowedModels: allowed,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created key %d, %q.\n\n    %s\n\n", id, strings.TrimSpace(*name), secret)
	fmt.Fprintln(stdout, "Store it now: it won't be shown again. Only its hash is kept.")
	return nil
}

// parseModels turns the -models flag into the key's allowed aliases, nil
// for every alias. Each one must exist in the config, so a typo fails here
// rather than as a 404 later.
func parseModels(flagValue, configPath string) ([]string, error) {
	if strings.TrimSpace(flagValue) == "" {
		return nil, nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("keys create: -models is checked against the config: %w", err)
	}
	var aliases []string
	for alias := range strings.SplitSeq(flagValue, ",") {
		alias = strings.TrimSpace(alias)
		if alias == "" || slices.Contains(aliases, alias) {
			continue
		}
		if _, ok := cfg.Models[alias]; !ok {
			return nil, fmt.Errorf("keys create: no model alias %q in %s", alias, configPath)
		}
		aliases = append(aliases, alias)
	}
	if len(aliases) == 0 {
		return nil, errors.New("keys create: -models lists no aliases")
	}
	return aliases, nil
}

func keysList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	list, err := s.ListKeys(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tPREFIX\tRPM\tTPM\tMODELS\tCREATED\tREVOKED")
	for _, k := range list {
		models := "all"
		if k.AllowedModels != nil {
			models = strings.Join(k.AllowedModels, ",")
		}
		revoked := "-"
		if k.RevokedAt != nil {
			revoked = k.RevokedAt.UTC().Format(time.DateTime)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			k.ID, k.Name, k.Prefix, k.RPM, k.TPM, models, k.CreatedAt.UTC().Format(time.DateTime), revoked)
	}
	return w.Flush()
}

func keysRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.Int64("id", 0, "the ID of the key to revoke, from `tollgate keys list` (required)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id < 1 {
		return errors.New("keys revoke: -id is required")
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	at, err := s.RevokeKey(ctx, *id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("keys revoke: there is no key %d", *id)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Key %d revoked at %s. A running gateway stops accepting it within auth.cache_ttl.\n",
		*id, at.UTC().Format(time.DateTime))
	return nil
}
