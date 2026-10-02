package store

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRunAsLeaderRetriesAnUnreachableDatabase: a standby whose owner pool
// cannot reach the database, as during a failover, keeps trying for the
// lock rather than ending the process. The pool opens lazily, so a port
// nothing listens on fails at acquire, not at construction.
func TestRunAsLeaderRetriesAnUnreachableDatabase(t *testing.T) {
	owner, err := pgxpool.New(t.Context(), "postgres://kritika@127.0.0.1:1/kritika?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	s := &Store{owner: owner, logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	led := false
	err = s.RunAsLeader(ctx, 20*time.Millisecond, func(context.Context) error {
		led = true
		return nil
	})
	if err != nil {
		t.Fatalf("RunAsLeader returned %v, want nil: the lock is retried", err)
	}
	if led {
		t.Fatal("lead ran without the lock")
	}
}
