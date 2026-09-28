//go:build integration

package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// insertReview creates a pull_requests row (with a randomised, collision-safe
// number, matching the convention TestGatewayTokens already established for
// this UNIQUE(repository_id, number) constraint) and a reviews row on top of
// it, both owned by account. It returns the review's id.
func insertReview(t *testing.T, ctx context.Context, s *Store, account string) string {
	t.Helper()
	var reviewID string
	err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		var repoID string
		if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE account_id = $1 LIMIT 1`, account).Scan(&repoID); err != nil {
			return err
		}
		var prID string
		if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, head_sha)
			VALUES ($1, $2, (random() * 1e6)::int, 'abc') RETURNING id`, account, repoID).Scan(&prID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status)
			VALUES ($1, $2, 'abc', 'running') RETURNING id`, account, prID).Scan(&reviewID)
	})
	if err != nil {
		t.Fatalf("insert review: %v", err)
	}
	return reviewID
}

// TestListenPublishesReviewEvents exercises the notify.go/0001_init.sql
// contract end to end: a reviews.status change must produce an Event on the
// kritik_events channel that Listen decodes and hands to onEvent.
func TestListenPublishesReviewEvents(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := accountID(t, s, "alpha")
	reviewID := insertReview(t, ctx, s, alpha)

	events := make(chan Event, 10)
	listenCtx := t.Context()
	go s.Listen(listenCtx, ListenHandlers{OnEvent: func(e Event) { events <- e }})
	// Postgres only delivers NOTIFY to sessions already LISTENing at commit
	// time; give Listen's connection a moment to register before the write.
	time.Sleep(250 * time.Millisecond)

	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET status = 'completed' WHERE id = $1`, reviewID)
		return err
	}); err != nil {
		t.Fatalf("update review status: %v", err)
	}

	select {
	case e := <-events:
		if e.Kind != EventReview || e.ID != reviewID || e.AccountID != alpha {
			t.Fatalf("event = %+v, want kind=review id=%s account=%s", e, reviewID, alpha)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no review event received for the status change")
	}
}

// TestListenSkipsRunnerRunHeartbeatOnlyUpdates checks the WHEN clause on
// kritik_notify_runner_run: a heartbeat-only update must not notify, while a
// phase change (the positive control, proving the listener itself works)
// must, and must not fire more than once for it.
func TestListenSkipsRunnerRunHeartbeatOnlyUpdates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := accountID(t, s, "alpha")
	var runID string
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, kind) VALUES ($1, 'index') RETURNING id`, alpha).Scan(&runID)
	}); err != nil {
		t.Fatalf("insert runner_runs: %v", err)
	}

	events := make(chan Event, 10)
	listenCtx := t.Context()
	go s.Listen(listenCtx, ListenHandlers{OnEvent: func(e Event) { events <- e }})
	time.Sleep(250 * time.Millisecond)

	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET heartbeat_at = now() WHERE id = $1`, runID)
		return err
	}); err != nil {
		t.Fatalf("update heartbeat_at: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("heartbeat-only update must not notify, got %+v", e)
	case <-time.After(500 * time.Millisecond):
	}

	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = 'fetching' WHERE id = $1`, runID)
		return err
	}); err != nil {
		t.Fatalf("update phase: %v", err)
	}
	select {
	case e := <-events:
		if e.Kind != EventRunnerRun || e.ID != runID {
			t.Fatalf("event = %+v, want kind=runner_run id=%s", e, runID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no runner_run event received for the phase change (listener not working?)")
	}
	// The phase change above must produce exactly one event, not a spurious
	// second one (e.g. a stray heartbeat notification re-delivered).
	select {
	case e := <-events:
		t.Fatalf("unexpected second event after the phase change: %+v", e)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestListenPublishesConfigEvents checks kritik_notify_config: an insert or
// update of the instance spec must call onConfig with its revision.
func TestListenPublishesConfigEvents(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	resetInstanceSpec(t, s)

	configs := make(chan string, 10)
	listenCtx := t.Context()
	go s.Listen(listenCtx, ListenHandlers{OnConfig: func(rev string) { configs <- rev }})
	time.Sleep(250 * time.Millisecond)

	for _, step := range []struct{ sql, want string }{
		{`INSERT INTO instance_config (id, spec, revision) VALUES (1, '{}'::jsonb, 1)`, "1"},
		{`UPDATE instance_config SET revision = revision + 1`, "2"},
	} {
		if _, err := s.owner.Exec(ctx, step.sql); err != nil {
			t.Fatalf("%s: %v", step.sql, err)
		}
		select {
		case rev := <-configs:
			if rev != step.want {
				t.Fatalf("config event = %q, want %q", rev, step.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no config event for %s", step.sql)
		}
	}
}

// TestModelCallsRowLevelSecurity checks that model_calls, the one new web
// table that is account content, gets the same account_isolation treatment as
// every other account-scoped table.
func TestModelCallsRowLevelSecurity(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := accountID(t, s, "alpha"), accountID(t, s, "beta")

	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO model_calls (account_id, kind, model) VALUES ($1, 'review', 'acme/large')`, alpha)
		return err
	}); err != nil {
		t.Fatalf("insert model_calls: %v", err)
	}

	count := func(account string) int {
		t.Helper()
		var n int
		if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM model_calls`).Scan(&n)
		}); err != nil {
			t.Fatalf("count model_calls: %v", err)
		}
		return n
	}
	if n := count(alpha); n != 1 {
		t.Fatalf("alpha sees %d model_calls rows, want 1", n)
	}
	if n := count(beta); n != 0 {
		t.Fatalf("beta sees %d model_calls rows, want 0 (RLS leak)", n)
	}

	// A foreign account_id must fail the WITH CHECK policy, same as every
	// other account-scoped table.
	err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO model_calls (account_id, kind, model) VALUES ($1, 'review', 'acme/large')`, beta)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("insert into model_calls with a foreign account_id: err = %v, want a 42501 permission-denied error", err)
	}
}

// TestRunnerRoleCannotTouchWebTables checks that the web dashboard's
// instance-level tables (no RLS; access control lives in web code) are not
// among the tables grant() gives the runner role, since a compromised
// runner container should never be able to read or write users,
// sessions, or any other dashboard table.
func TestRunnerRoleCannotTouchWebTables(t *testing.T) {
	openStore(t) // ensures Migrate/grant() have run against this schema
	ctx := context.Background()
	runner, err := Open(ctx, Options{
		AppURL: testEnv(t, "KRITIK_TEST_RUNNER_URL"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open runner store: %v", err)
	}
	t.Cleanup(runner.Close)

	tables := []string{
		"users", "identities", "sessions", "login_states",
		"audit_events", "instance_config", "model_calls",
	}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			var n int
			err := runner.app.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("runner querying %s: err = %v, want a 42501 permission-denied error", table, err)
			}
		})
	}
}
