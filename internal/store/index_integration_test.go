//go:build integration

package store

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"
)

const onboardTenants = twoTenants + `
  - slug: east
    installations:
      - name: east-bot
        forge: forgejo
        account: east
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: east/busy
      - name: east/quiet
      - name: east/later
      - name: east/rebuilt
      - name: east/skipped
      - name: east/failed
      - name: east/queued
      - name: east/indexed
      - name: east/off
        enabled: false
  - slug: west
    installations:
      - name: west-bot
        forge: forgejo
        account: west
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: west/one
      - name: west/old-failure
`

// TestOnboardCandidates checks which repositories the onboarding feeder is
// offered and in what order: tenants take turns, and a tenant's repositories
// whose pull requests moved last go first.
func TestOnboardCandidates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, onboardTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	ids := map[string]string{}
	names := map[string]string{}
	rows, err := s.owner.Query(ctx, `SELECT id, name FROM repositories WHERE name LIKE 'east/%' OR name LIKE 'west/%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		ids[name], names[id] = id, name
	}
	if rows.Err() != nil || len(ids) != 11 {
		t.Fatalf("repositories = %v, %v", ids, rows.Err())
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `DELETE FROM river_job WHERE args->>'repository_id' = ANY($1)`, slices.Collect(maps.Values(ids)))
	})
	// One apply gives every repository the same created_at.
	for name, age := range map[string]string{"east/quiet": "3 hours", "west/old-failure": "2 hours", "east/later": "1 hour", "east/rebuilt": "30 minutes"} {
		exec(`UPDATE repositories SET created_at = now() - $2::interval WHERE id = $1`, ids[name], age)
	}
	for name, age := range map[string]string{"east/busy": "1 minute", "west/one": "1 day"} {
		exec(`INSERT INTO pull_requests (tenant_id, repository_id, number, head_sha, updated_at)
			SELECT tenant_id, id, 1, 'abc', now() - $2::interval FROM repositories WHERE id = $1`, ids[name], age)
	}
	exec(`WITH g AS (
			INSERT INTO index_runs (tenant_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
			SELECT tenant_id, id, 'abc', 'm', 8, 'full', 'completed' FROM repositories WHERE id = $1 RETURNING id, repository_id)
		UPDATE repositories r SET active_index_run_id = g.id FROM g WHERE r.id = g.repository_id`, ids["east/indexed"])

	before, err := s.OnboardingInFlight(ctx)
	if err != nil {
		t.Fatalf("OnboardingInFlight: %v", err)
	}
	// job queues an index job, or with a finished age, one that finished
	// that long ago.
	job := func(name, trigger, state, finished string) {
		t.Helper()
		exec(`INSERT INTO river_job (kind, queue, args, max_attempts, state, finalized_at)
			VALUES ('index', 'index', jsonb_build_object('repository_id', $1::text, 'trigger', $2::text), 3, $3,
				now() - nullif($4, '')::interval)`,
			ids[name], trigger, state, finished)
	}
	job("east/queued", "push", "running", "")
	job("east/failed", "onboard", "discarded", "10 minutes")
	job("west/old-failure", "onboard", "discarded", "2 hours")
	job("east/later", "push", "completed", "1 minute")
	job("east/skipped", "onboard", "completed", "5 minutes")
	// An onboarding that built an index since dropped, as a model change
	// drops them all, is no reason to wait.
	job("east/rebuilt", "onboard", "completed", "5 minutes")
	exec(`INSERT INTO index_runs (tenant_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
		SELECT tenant_id, id, 'abc', 'm', 8, 'full', 'superseded' FROM repositories WHERE id = $1`, ids["east/rebuilt"])
	after, err := s.OnboardingInFlight(ctx)
	if err != nil || after != before {
		t.Fatalf("OnboardingInFlight = %d, %v; want %d: pushes and finished jobs are not onboarding", after, err, before)
	}
	job("east/queued", "onboard", "retryable", "")
	if after, err := s.OnboardingInFlight(ctx); err != nil || after != before+1 {
		t.Fatalf("OnboardingInFlight = %d, %v; want %d", after, err, before+1)
	}

	refs, err := s.OnboardCandidates(ctx, 1000, time.Hour)
	if err != nil {
		t.Fatalf("OnboardCandidates: %v", err)
	}
	var got []string
	for _, r := range refs {
		if name, ok := names[r.ID]; ok {
			got = append(got, name)
		}
	}
	want := []string{"east/busy", "west/one", "east/quiet", "west/old-failure", "east/later", "east/rebuilt"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if limited, err := s.OnboardCandidates(ctx, 1, time.Hour); err != nil || len(limited) != 1 {
		t.Fatalf("OnboardCandidates(1) = %v, %v", limited, err)
	}
}
