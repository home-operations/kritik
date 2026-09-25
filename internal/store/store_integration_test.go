//go:build integration

package store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/home-operations/kritik/internal/configfile"
)

// The suite needs a VectorChord-enabled Postgres with three roles, as
// `mise run test-integration` provisions:
//   KRITIK_TEST_OWNER_URL   database owner (not superuser)
//   KRITIK_TEST_APP_URL     application role, owns nothing
//   KRITIK_TEST_RUNNER_URL  runner role, owns nothing
//   KRITIK_TEST_SUPER_URL   a superuser, used only to assert refusal

func testEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

func openStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, Options{
		AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_OWNER_URL"),
		AppRole: "kritik_app", RunnerRole: "kritik_runner",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestOpenRefusesUnsafeApplicationDSN(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		url  string
		want error
	}{
		{"superuser as application role", testEnv(t, "KRITIK_TEST_SUPER_URL"), ErrIsolationOff},
		{"owner as application role", testEnv(t, "KRITIK_TEST_OWNER_URL"), ErrIsolationOff},
	}
	// The owner must own at least one table for the ownership check to bite.
	openStore(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(ctx, Options{AppURL: tt.url, AppRole: "kritik_app", RunnerRole: "kritik_runner", Logger: logger})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Open = %v, want %v", err, tt.want)
			}
		})
	}
	t.Run("superuser as owner", func(t *testing.T) {
		_, err := Open(ctx, Options{AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_SUPER_URL"), Logger: logger})
		if err == nil || !strings.Contains(err.Error(), "superuser") {
			t.Fatalf("Open = %v, want a superuser refusal", err)
		}
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openStore(t)
	if ready, err := s.SchemaReady(context.Background()); err != nil || !ready {
		t.Fatalf("SchemaReady after Migrate = %v, %v", ready, err)
	}
	if err := s.Migrate(context.Background(), "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	names, _ := fs.Glob(migrationFS, "migrations/*.sql")
	var n int
	if err := s.owner.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(names) {
		t.Fatalf("schema_migrations rows = %d, err %v; want one per embedded migration (%d)", n, err, len(names))
	}
}

const twoTenants = `
tenants:
  - slug: alpha
    installations:
      - name: alpha-bot
        forge: forgejo
        account: alpha
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
    repositories:
      - name: alpha/one
      - name: alpha/two
        enabled: false
  - slug: beta
    installations:
      - name: beta-bot
        forge: forgejo
        account: beta
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
`

func parse(t *testing.T, yaml string) *configfile.File {
	t.Helper()
	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	f, err := configfile.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func tenantID(t *testing.T, s *Store, slug string) string {
	t.Helper()
	var id string
	if err := s.owner.QueryRow(context.Background(), `SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&id); err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	return id
}

func TestApplyConfigAndRowLevelSecurity(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := parse(t, twoTenants)
	if err := s.ApplyConfig(ctx, f, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if got, _ := s.AppliedConfigHash(ctx); got != f.Hash() {
		t.Fatalf("applied hash = %q, want %q", got, f.Hash())
	}
	alpha, beta := tenantID(t, s, "alpha"), tenantID(t, s, "beta")

	count := func(t *testing.T, tenant, table string) int {
		t.Helper()
		var n int
		err := s.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		if err != nil {
			t.Fatalf("count %s as %s: %v", table, tenant, err)
		}
		return n
	}

	t.Run("each tenant sees only its own rows", func(t *testing.T) {
		if count(t, alpha, "tenants") != 1 || count(t, alpha, "installations") != 1 || count(t, alpha, "repositories") != 2 {
			t.Fatal("alpha should see its own tenant, installation and two repositories")
		}
		if count(t, beta, "repositories") != 0 || count(t, beta, "installations") != 1 {
			t.Fatal("beta should see one installation and no repositories")
		}
	})

	t.Run("no tenant set sees nothing", func(t *testing.T) {
		for _, table := range []string{"tenants", "installations", "repositories", "model_leases"} {
			var n int
			if err := s.app.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("%s: %d rows visible with no tenant set", table, n)
			}
		}
	})

	t.Run("tenant does not leak across pooled connections", func(t *testing.T) {
		// Exhaust the pool's connections through WithTenant, then query bare.
		for range 20 {
			if count(t, alpha, "repositories") != 2 {
				t.Fatal("alpha count changed")
			}
		}
		var n int
		if err := s.app.QueryRow(ctx, `SELECT count(*) FROM repositories`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("bare query after tenant transactions saw %d rows (err %v)", n, err)
		}
	})

	t.Run("application role cannot insert into another tenant", func(t *testing.T) {
		err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO model_leases (tenant_id, model_key, slot) VALUES ($1, 'p/m', 0)`, beta)
			return err
		})
		if err == nil {
			t.Fatal("insert with a foreign tenant_id must fail the WITH CHECK policy")
		}
	})

	t.Run("disabled repository is recorded as disabled", func(t *testing.T) {
		var enabled bool
		var disabledAt *time.Time
		if err := s.owner.QueryRow(ctx, `SELECT enabled, disabled_at FROM repositories WHERE name = 'alpha/two'`).Scan(&enabled, &disabledAt); err != nil {
			t.Fatal(err)
		}
		if enabled || disabledAt == nil {
			t.Fatalf("alpha/two enabled=%v disabled_at=%v", enabled, disabledAt)
		}
	})

	t.Run("removing a tenant from the file disables it and keeps its rows", func(t *testing.T) {
		f2 := parse(t, strings.SplitN(twoTenants, "  - slug: beta", 2)[0])
		if err := s.ApplyConfig(ctx, f2, "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		var enabled bool
		var rows int
		if err := s.owner.QueryRow(ctx, `SELECT enabled, (SELECT count(*) FROM installations WHERE tenant_id = tenants.id) FROM tenants WHERE slug = 'beta'`).Scan(&enabled, &rows); err != nil {
			t.Fatal(err)
		}
		if enabled || rows != 1 {
			t.Fatalf("beta enabled=%v installations=%d; want disabled with rows kept", enabled, rows)
		}
		var instEnabled bool
		if err := s.owner.QueryRow(ctx, `SELECT enabled FROM installations WHERE name = 'beta-bot'`).Scan(&instEnabled); err != nil || instEnabled {
			t.Fatalf("beta-bot enabled=%v err=%v; want disabled", instEnabled, err)
		}
		if err := s.ApplyConfig(ctx, f, "test"); err != nil {
			t.Fatalf("re-apply: %v", err)
		}
		if err := s.owner.QueryRow(ctx, `SELECT enabled FROM tenants WHERE slug = 'beta'`).Scan(&enabled); err != nil || !enabled {
			t.Fatalf("beta re-enabled=%v err=%v", enabled, err)
		}
	})
}

func TestRunnerRoleUpdatesOnlyWhatARunnerReports(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := tenantID(t, s, "alpha"), tenantID(t, s, "beta")
	var runID string
	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, kind) VALUES ($1, 'index') RETURNING id`, alpha).Scan(&runID)
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := Open(ctx, Options{AppURL: testEnv(t, "KRITIK_TEST_RUNNER_URL"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Open runner: %v", err)
	}
	t.Cleanup(runner.Close)
	tests := []struct {
		name    string
		stmt    string
		allowed bool
	}{
		{name: "phase", stmt: `UPDATE runner_runs SET phase = 'fetching' WHERE id = $1`, allowed: true},
		{name: "heartbeat", stmt: `UPDATE runner_runs SET heartbeat_at = now() WHERE id = $1`, allowed: true},
		{name: "error", stmt: `UPDATE runner_runs SET phase = 'failed', error = 'boom' WHERE id = $1`, allowed: true},
		{name: "tenant", stmt: `UPDATE runner_runs SET tenant_id = '` + beta + `' WHERE id = $1`},
		{name: "log tail", stmt: `UPDATE runner_runs SET log_tail = 'forged' WHERE id = $1`},
		{name: "exit code", stmt: `UPDATE runner_runs SET exit_code = 0 WHERE id = $1`},
		{name: "secret swept", stmt: `UPDATE runner_runs SET secret_swept_at = now() WHERE id = $1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runner.WithRunnerJob(ctx, runID, func(tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, tt.stmt, runID)
				if err == nil && tag.RowsAffected() != 1 {
					return errors.New("no row updated")
				}
				return err
			})
			var pgErr *pgconn.PgError
			switch {
			case tt.allowed && err != nil:
				t.Fatalf("a runner must be able to update its %s: %v", tt.name, err)
			case !tt.allowed && (!errors.As(err, &pgErr) || pgErr.Code != "42501"):
				t.Fatalf("a runner updating its %s must be refused permission, got %v", tt.name, err)
			}
		})
	}
	var tenant string
	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id FROM runner_runs WHERE id = $1`, runID).Scan(&tenant)
	}); err != nil || tenant != alpha {
		t.Fatalf("run tenant = %q, %v", tenant, err)
	}
}

func TestRunSecretsToSweep(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := tenantID(t, s, "alpha"), tenantID(t, s, "beta")
	insert := func(tenant, age string, finished, swept bool) string {
		t.Helper()
		var id string
		if err := s.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, kind, created_at, finished_at, secret_swept_at)
				VALUES ($1, 'index', now() - $2::interval,
					CASE WHEN $3 THEN now() END, CASE WHEN $4 THEN now() END) RETURNING id`,
				tenant, age, finished, swept).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	finishedOld := insert(alpha, "20 minutes", true, false)
	abandoned := insert(alpha, "4 hours", false, false)
	insert(alpha, "5 minutes", true, false)   // too fresh
	insert(alpha, "20 minutes", false, false) // may still be running
	insert(alpha, "20 minutes", true, true)   // already swept
	betaRun := insert(beta, "20 minutes", true, false)

	got, err := s.RunSecretsToSweep(ctx, alpha, 15*time.Minute, 3*time.Hour, 100)
	if err != nil {
		t.Fatalf("RunSecretsToSweep: %v", err)
	}
	if want := []string{abandoned, finishedOld}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("alpha runs to sweep = %v, want %v (oldest first, none of beta's)", got, want)
	}
	if limited, err := s.RunSecretsToSweep(ctx, alpha, 15*time.Minute, 3*time.Hour, 1); err != nil || len(limited) != 1 || limited[0] != abandoned {
		t.Fatalf("limited sweep = %v, %v; want the oldest run only", limited, err)
	}
	// Marking is idempotent and scoped to the tenant: beta's run is not
	// visible from alpha, so marking it there changes nothing.
	for range 2 {
		if err := s.MarkRunSecretsSwept(ctx, alpha, append(got, betaRun)); err != nil {
			t.Fatalf("MarkRunSecretsSwept: %v", err)
		}
	}
	if left, err := s.RunSecretsToSweep(ctx, alpha, 15*time.Minute, 3*time.Hour, 100); err != nil || len(left) != 0 {
		t.Fatalf("alpha after marking = %v, %v; want none", left, err)
	}
	if left, err := s.RunSecretsToSweep(ctx, beta, 15*time.Minute, 3*time.Hour, 100); err != nil || len(left) != 1 || left[0] != betaRun {
		t.Fatalf("beta after alpha's marking = %v, %v; want its own run", left, err)
	}
}

func TestLeaderLockIsExclusive(t *testing.T) {
	s := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s.RunAsLeader(ctx, 50*time.Millisecond, func(ctx context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("first leader never acquired the lock")
	}

	second := openStore(t)
	got := make(chan struct{}, 1)
	ctx2 := t.Context()
	go func() {
		_ = second.RunAsLeader(ctx2, 50*time.Millisecond, func(ctx context.Context) error {
			got <- struct{}{}
			<-ctx.Done()
			return nil
		})
	}()
	select {
	case <-got:
		t.Fatal("second replica became leader while the first held the lock")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	cancel()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second replica did not take over after the first released")
	}
}

func TestMain(m *testing.M) {
	// Each run starts from an empty schema so the suite is repeatable.
	if super := os.Getenv("KRITIK_TEST_SUPER_URL"); super != "" {
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, super)
		if err == nil {
			_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;
				GRANT ALL ON SCHEMA public TO kritik; GRANT USAGE ON SCHEMA public TO kritik_app, kritik_runner;
				CREATE EXTENSION IF NOT EXISTS vchord CASCADE`)
			pool.Close()
		}
	}
	os.Exit(m.Run())
}

var _ = filepath.Join

// TestEnsureIndexSchemaUsesVectorChord checks that the embedding index is a
// vchordrq one and that the application role can turn on the prefilter the
// similarity query sets.
func TestEnsureIndexSchemaUsesVectorChord(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	if err := s.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// The suites share one database: leave no index schema behind.
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `DROP TABLE IF EXISTS index_chunks; DELETE FROM index_schema`)
	})
	if err := s.EnsureIndexSchema(ctx, "kritik_app", "test-embed", 8, true); err != nil {
		t.Fatalf("EnsureIndexSchema: %v", err)
	}
	var method string
	if err := s.owner.QueryRow(ctx, `SELECT am.amname FROM pg_class c JOIN pg_am am ON am.oid = c.relam
		WHERE c.relname = 'index_chunks_embedding_idx'`).Scan(&method); err != nil {
		t.Fatalf("embedding index: %v", err)
	}
	if method != "vchordrq" {
		t.Fatalf("index method = %q, want vchordrq", method)
	}
	if _, err := s.app.Exec(ctx, `SET vchordrq.prefilter = on`); err != nil {
		t.Fatalf("the application role must be able to set the prefilter: %v", err)
	}
}
