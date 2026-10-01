// Package storetest opens the store on the integration tests' database,
// the one the test-integration task provisions.
package storetest

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/home-operations/kritika/internal/store"
)

// Env is the environment variable key, skipping the test when it is
// unset.
func Env(t testing.TB, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

// Open opens the store on KRITIKA_TEST_APP_URL and KRITIKA_TEST_OWNER_URL,
// migrated for the kritika_app and kritika_runner roles and closed when the
// test ends.
func Open(t testing.TB) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		AppURL: Env(t, "KRITIKA_TEST_APP_URL"), OwnerURL: Env(t, "KRITIKA_TEST_OWNER_URL"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritika_app", "kritika_runner"); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return st
}
