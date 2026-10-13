package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/store"
	"github.com/ktripathi2281/tollgate/internal/store/storetest"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// These tests need Postgres: see storetest.EnvURL.

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestMigrateAndCheckSchema(t *testing.T) {
	ctx := t.Context()
	s, err := store.Open(ctx, storetest.EmptySchemaURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.CheckSchema(ctx); err == nil {
		t.Error("CheckSchema on an empty schema: want an error")
	}
	applied, version, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) == 0 || version < 1 {
		t.Errorf("Migrate applied %v, version %d; want at least one migration", applied, version)
	}
	if err := s.CheckSchema(ctx); err != nil {
		t.Errorf("CheckSchema after Migrate: %v", err)
	}

	// Running it again applies nothing.
	again, version2, err := s.Migrate(ctx)
	if err != nil || len(again) != 0 || version2 != version {
		t.Errorf("second Migrate = %v, %d, %v; want nothing applied, version %d", again, version2, err, version)
	}
}

func TestOpenFailsFast(t *testing.T) {
	_, err := store.Open(t.Context(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err == nil {
		t.Fatal("Open with nothing listening: want an error")
	}
	if _, err := store.Open(t.Context(), "not a url ::"); err == nil {
		t.Error("Open with a malformed URL: want an error")
	}
}

func newKey(t *testing.T, name string, models []string) (store.NewKey, string) {
	t.Helper()
	secret := auth.Generate()
	return store.NewKey{
		Name: name, Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret),
		RPM: 60, TPM: 1000, AllowedModels: models,
	}, secret
}

func TestKeys(t *testing.T) {
	ctx := t.Context()
	s := storetest.Open(t)

	first, firstSecret := newKey(t, "first", nil)
	id1, created, err := s.CreateKey(ctx, first)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if created.IsZero() {
		t.Error("CreateKey returned no creation time")
	}
	second, _ := newKey(t, "second", []string{"mock-fast", "fast"})
	id2, _, err := s.CreateKey(ctx, second)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	got, err := s.KeyByHash(ctx, auth.Hash(firstSecret))
	if err != nil {
		t.Fatalf("KeyByHash: %v", err)
	}
	want := auth.Key{ID: id1, Name: "first", Prefix: first.Prefix, RPM: 60, TPM: 1000}
	if got.ID != want.ID || got.Name != want.Name || got.Prefix != want.Prefix ||
		got.RPM != want.RPM || got.TPM != want.TPM || got.AllowedModels != nil || got.Revoked {
		t.Errorf("KeyByHash = %+v, want %+v", got, want)
	}
	if _, err := s.KeyByHash(ctx, auth.Hash(auth.Generate())); !errors.Is(err, auth.ErrUnknownKey) {
		t.Errorf("KeyByHash of an unknown key: got %v, want auth.ErrUnknownKey", err)
	}

	list, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(list) != 2 || list[0].ID != id1 || list[1].ID != id2 {
		t.Fatalf("ListKeys = %+v, want keys %d and %d in order", list, id1, id2)
	}
	if !slices.Equal(list[1].AllowedModels, []string{"mock-fast", "fast"}) {
		t.Errorf("allowed models = %v, want [mock-fast fast]", list[1].AllowedModels)
	}

	revokedAt, err := s.RevokeKey(ctx, id1)
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if got, _ := s.KeyByHash(ctx, auth.Hash(firstSecret)); !got.Revoked {
		t.Error("a revoked key is returned without Revoked set")
	}
	again, err := s.RevokeKey(ctx, id1)
	if err != nil || !again.Equal(revokedAt) {
		t.Errorf("revoking twice = %v, %v; want the first time, %v", again, err, revokedAt)
	}
	if _, err := s.RevokeKey(ctx, 999_999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking a missing key: got %v, want store.ErrNotFound", err)
	}
}

func TestCreateKeyRejectsBadRows(t *testing.T) {
	ctx := t.Context()
	s := storetest.Open(t)

	k, _ := newKey(t, "dup", nil)
	if _, _, err := s.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey(ctx, k); err == nil {
		t.Error("a second key with the same hash: want an error")
	}

	tests := []struct {
		name   string
		mutate func(*store.NewKey)
	}{
		{"empty allowed list", func(k *store.NewKey) { k.AllowedModels = []string{} }},
		{"zero rpm", func(k *store.NewKey) { k.RPM = 0 }},
		{"zero tpm", func(k *store.NewKey) { k.TPM = 0 }},
		{"empty name", func(k *store.NewKey) { k.Name = "" }},
		{"rpm beyond int32", func(k *store.NewKey) { k.RPM = 1 << 31 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, _ := newKey(t, "bad", nil)
			tt.mutate(&k)
			if _, _, err := s.CreateKey(context.Background(), k); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestWriteRequestLogs(t *testing.T) {
	s, dbURL := storetest.OpenWithURL(t)
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	entries := []usage.Entry{
		{
			Time: at, RequestID: "req_ok", KeyID: 7, Alias: "fast", Provider: "anthropic", Model: "claude-haiku-5-5",
			Stream: true, Status: 200, Attempts: 2, PromptTokens: 10, CompletionTokens: 20,
			Cost: 123, Latency: 1500 * time.Millisecond, TTFT: 250 * time.Millisecond,
		},
		// A request refused before it was read: most columns are empty.
		{Time: at, RequestID: "req_401", Status: 401, ErrorCode: "invalid_api_key", Latency: time.Millisecond},
	}
	if err := s.WriteRequestLogs(t.Context(), entries); err != nil {
		t.Fatalf("WriteRequestLogs: %v", err)
	}
	if n := storetest.Int(t, dbURL, "SELECT count(*) FROM request_logs"); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
	if n := storetest.Int(t, dbURL, `SELECT count(*) FROM request_logs WHERE request_id = 'req_ok'
		AND key_id = 7 AND alias = 'fast' AND provider = 'anthropic' AND stream AND status = 200 AND attempts = 2
		AND prompt_tokens = 10 AND completion_tokens = 20 AND cost_micros = 123
		AND latency_us = 1500000 AND ttft_us = 250000 AND error_code IS NULL AND NOT replayed`); n != 1 {
		t.Error("the successful row's columns don't match")
	}
	if n := storetest.Int(t, dbURL, `SELECT count(*) FROM request_logs WHERE request_id = 'req_401'
		AND key_id IS NULL AND alias IS NULL AND provider IS NULL AND ttft_us IS NULL AND error_code = 'invalid_api_key'`); n != 1 {
		t.Error("empty fields should be NULL")
	}
	if err := s.Ping(t.Context()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
