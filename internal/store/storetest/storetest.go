// Package storetest gives tests a database schema of their own, migrated
// and dropped when the test ends, so tests can run in parallel against one
// Postgres.
package storetest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ktripathi2281/tollgate/internal/store"
)

// EnvURL names the variable holding the test database's URL. Tests that
// need Postgres are skipped without it, except in CI, where they fail so
// they can't be skipped by accident.
const EnvURL = "TOLLGATE_TEST_DATABASE_URL"

// URL returns the URL of a new, migrated schema in the test database. The
// schema is dropped when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	u := EmptySchemaURL(t)
	s, err := store.Open(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return u
}

// EmptySchemaURL returns the URL of a new schema with no tables, dropped
// when the test ends.
func EmptySchemaURL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvURL)
	if base == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", EnvURL)
		}
		t.Skipf("%s is not set; this test needs Postgres", EnvURL)
	}

	// t.Context is cancelled before cleanups run, so the setup and the
	// cleanup use a context of their own.
	ctx := context.Background()
	schema := "test_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	exec(t, ctx, base, "CREATE SCHEMA "+quoted)
	t.Cleanup(func() { exec(t, ctx, base, "DROP SCHEMA "+quoted+" CASCADE") })
	return withSearchPath(t, base, schema)
}

// Open returns a Store on a new, migrated schema (see URL), closed when the
// test ends.
func Open(t testing.TB) *store.Store {
	t.Helper()
	s, _ := OpenWithURL(t)
	return s
}

// OpenWithURL is Open, and also returns the schema's URL.
func OpenWithURL(t testing.TB) (*store.Store, string) {
	t.Helper()
	u := URL(t)
	s, err := store.Open(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, u
}

// Int runs a query that returns one integer, such as a count, on the
// database at dbURL.
func Int(t testing.TB, dbURL, sql string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	defer conn.Close(ctx)
	var n int64
	if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// CheckBudgets fails the test unless every budget period agrees with its
// holds, as `tollgate reconcile` checks: invariants 1 and 2 in brief
// section 9.1. It also fails if any hold is still held, since a test calls
// it once every request has finished.
func CheckBudgets(t testing.TB, s *store.Store, dbURL string) {
	t.Helper()
	checks, err := s.ReconcileBudgets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if !c.OK() {
			t.Errorf("budget period doesn't match its holds: %+v", c)
		}
	}
	if n := Int(t, dbURL, "SELECT count(*) FROM budget_holds WHERE status = 'held'"); n != 0 {
		t.Errorf("%d holds are still held", n)
	}
}

// exec runs one statement on a connection of its own.
func exec(t testing.TB, ctx context.Context, dbURL, sql string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// withSearchPath adds search_path to a postgres:// URL. pgx sends
// parameters it doesn't know to the server as settings, so every
// connection, goose's included, uses the test's schema.
func withSearchPath(t testing.TB, dbURL, schema string) string {
	t.Helper()
	u, err := url.Parse(dbURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// URL", EnvURL)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
