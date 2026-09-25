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
// it, both owned by tenant. It returns the review's id.
func insertReview(t *testing.T, ctx context.Context, s *Store, tenant string) string {
	t.Helper()
	var reviewID string
	err := s.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
		var repoID string
		if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(&repoID); err != nil {
			return err
		}
		var prID string
		if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (tenant_id, repository_id, number, head_sha)
			VALUES ($1, $2, (random() * 1e6)::int, 'abc') RETURNING id`, tenant, repoID).Scan(&prID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO reviews (tenant_id, pull_request_id, head_sha, status)
			VALUES ($1, $2, 'abc', 'running') RETURNING id`, tenant, prID).Scan(&reviewID)
	})
	if err != nil {
		t.Fatalf("insert review: %v", err)
	}
	return reviewID
}

// TestListenPublishesReviewEvents exercises the notify.go/0004_web.sql
// contract end to end: a reviews.status change must produce an Event on the
// kritik_events channel that Listen decodes and hands to onEvent.
func TestListenPublishesReviewEvents(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := tenantID(t, s, "alpha")
	reviewID := insertReview(t, ctx, s, alpha)

	events := make(chan Event, 10)
	listenCtx := t.Context()
	go func() { _ = s.Listen(listenCtx, func(e Event) { events <- e }, func(string) {}) }()
	// Postgres only delivers NOTIFY to sessions already LISTENing at commit
	// time; give Listen's connection a moment to register before the write.
	time.Sleep(250 * time.Millisecond)

	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET status = 'completed' WHERE id = $1`, reviewID)
		return err
	}); err != nil {
		t.Fatalf("update review status: %v", err)
	}

	select {
	case e := <-events:
		if e.Kind != EventReview || e.ID != reviewID || e.TenantID != alpha {
			t.Fatalf("event = %+v, want kind=review id=%s tenant=%s", e, reviewID, alpha)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no review event received for the status change")
	}
}

// TestListenSkipsRunnerRunHeartbeatOnlyUpdates checks the WHEN clause on
// kritik_notify_runner_run: a heartbeat-only update must not notify, while a
// phase change (the positive control, proving the listener itself works)
// must.
func TestListenSkipsRunnerRunHeartbeatOnlyUpdates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := tenantID(t, s, "alpha")
	var runID string
	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, kind) VALUES ($1, 'index') RETURNING id`, alpha).Scan(&runID)
	}); err != nil {
		t.Fatalf("insert runner_runs: %v", err)
	}

	events := make(chan Event, 10)
	listenCtx := t.Context()
	go func() { _ = s.Listen(listenCtx, func(e Event) { events <- e }, func(string) {}) }()
	time.Sleep(250 * time.Millisecond)

	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
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

	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
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
}

// TestListenPublishesConfigEvents checks kritik_notify_config: an insert or
// update on dashboard_tenants must call onConfig with the row's slug.
func TestListenPublishesConfigEvents(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	configs := make(chan string, 10)
	listenCtx := t.Context()
	go func() { _ = s.Listen(listenCtx, func(Event) {}, func(slug string) { configs <- slug }) }()
	time.Sleep(250 * time.Millisecond)

	// dashboard_tenants carries no RLS (read before the tenant it describes
	// exists), so the owner pool can write it directly.
	if _, err := s.owner.Exec(ctx, `INSERT INTO dashboard_tenants (slug, spec) VALUES ('gamma', '{}'::jsonb)`); err != nil {
		t.Fatalf("insert dashboard_tenants: %v", err)
	}
	select {
	case slug := <-configs:
		if slug != "gamma" {
			t.Fatalf("config slug = %q, want gamma", slug)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no config event received for the insert")
	}

	if _, err := s.owner.Exec(ctx, `UPDATE dashboard_tenants SET revision = revision + 1 WHERE slug = 'gamma'`); err != nil {
		t.Fatalf("update dashboard_tenants: %v", err)
	}
	select {
	case slug := <-configs:
		if slug != "gamma" {
			t.Fatalf("config slug = %q, want gamma", slug)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no config event received for the update")
	}
}

// TestModelCallsRowLevelSecurity checks that model_calls, the one new web
// table that is tenant content, gets the same tenant_isolation treatment as
// every other tenant-scoped table.
func TestModelCallsRowLevelSecurity(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := tenantID(t, s, "alpha"), tenantID(t, s, "beta")

	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO model_calls (tenant_id, kind, model) VALUES ($1, 'review', 'acme/large')`, alpha)
		return err
	}); err != nil {
		t.Fatalf("insert model_calls: %v", err)
	}

	count := func(tenant string) int {
		t.Helper()
		var n int
		if err := s.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
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

	// A foreign tenant_id must fail the WITH CHECK policy, same as every
	// other tenant-scoped table.
	err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO model_calls (tenant_id, kind, model) VALUES ($1, 'review', 'acme/large')`, beta)
		return err
	})
	if err == nil {
		t.Fatal("insert into model_calls with a foreign tenant_id must be refused")
	}
}

// TestRunnerRoleCannotTouchWebTables checks that the web dashboard's
// instance-level tables (no RLS; access control lives in web code) are not
// among the tables grant() gives the runner role, since a compromised
// runner container should never be able to read or write accounts,
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
		"accounts", "identities", "sessions", "login_states",
		"memberships", "invites", "audit_events", "dashboard_tenants", "model_calls",
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
