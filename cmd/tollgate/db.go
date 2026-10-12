package main

import (
	"context"
	"errors"
	"os"

	"github.com/ktripathi2281/tollgate/internal/store"
)

// databaseURLEnv names the variable with the database's URL. Like every
// secret, it comes from the environment, never the config file.
const databaseURLEnv = "DATABASE_URL"

// openStore connects to the database named by DATABASE_URL.
func openStore(ctx context.Context) (*store.Store, error) {
	url := os.Getenv(databaseURLEnv)
	if url == "" {
		return nil, errors.New(databaseURLEnv + " is not set")
	}
	return store.Open(ctx, url)
}
