// Package store is the gateway's Postgres access: the connection pool, the
// migrations embedded in the binary, and the typed queries that sqlc
// generates into package db.
package store

import (
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/ktripathi2281/tollgate/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// connectTimeout bounds the first connection, so a wrong address fails
// startup quickly instead of hanging.
const connectTimeout = 5 * time.Second

// Store is a pool of database connections and the queries that use it. It
// is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// Open connects to the database at url, a postgres:// URL. Pool settings
// can go in the URL too, for example pool_max_conns=10. Open fails if the
// database can't be reached.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx leaves the password out of its parse errors.
		return nil, fmt.Errorf("database URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return &Store{pool: pool, q: db.New(pool)}, nil
}

// Close closes every connection. Calls in progress finish first.
func (s *Store) Close() {
	s.pool.Close()
}

// Migrate applies every migration that hasn't been applied, and returns
// their names and the schema version afterwards. A Postgres advisory lock
// makes concurrent runs take turns.
func (s *Store) Migrate(ctx context.Context) (applied []string, version int64, err error) {
	p, err := s.migrator(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer p.Close()

	results, err := p.Up(ctx)
	for _, r := range results {
		if r.Error == nil {
			applied = append(applied, r.Source.Path)
		}
	}
	if err != nil {
		return applied, 0, fmt.Errorf("migrate: %w", err)
	}
	version, err = p.GetDBVersion(ctx)
	if err != nil {
		return applied, 0, fmt.Errorf("migrate: read the schema version: %w", err)
	}
	return applied, version, nil
}

// CheckSchema returns an error if any migration hasn't been applied, so
// the gateway never runs queries against an older schema.
func (s *Store) CheckSchema(ctx context.Context) error {
	p, err := s.migrator(ctx)
	if err != nil {
		return err
	}
	defer p.Close()

	pending, err := p.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("check the schema version: %w", err)
	}
	if pending {
		return fmt.Errorf("the database schema is out of date; run `tollgate migrate` first")
	}
	return nil
}

// migrator returns a goose provider over the embedded migrations. goose
// works on database/sql, so it gets a *sql.DB that borrows connections
// from the pool. Closing the provider closes that *sql.DB, not the pool.
//
// The advisory lock that makes concurrent migrations take turns is per
// schema: its ID is a hash of the schema's name. Runs against the same
// schema wait for each other; runs against different schemas, such as
// parallel tests, don't.
func (s *Store) migrator(ctx context.Context) (*goose.Provider, error) {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return nil, fmt.Errorf("migrations: read the current schema: %w", err)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("tollgate migrations " + schema))
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(int64(h.Sum64())))
	if err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(s.pool), fsys,
		goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return p, nil
}
