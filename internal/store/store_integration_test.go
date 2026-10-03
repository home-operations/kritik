//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
)

// The suite needs a VectorChord-enabled Postgres with three roles, as
// `mise run test-integration` provisions:
//   KRITIKA_TEST_OWNER_URL   database owner (not superuser)
//   KRITIKA_TEST_APP_URL     application role, owns nothing
//   KRITIKA_TEST_RUNNER_URL  runner role, owns nothing
//   KRITIKA_TEST_SUPER_URL   a superuser, used only to assert refusal

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
		AppURL: testEnv(t, "KRITIKA_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIKA_TEST_OWNER_URL"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx, "kritika_app", "kritika_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// TestPoolStatementTimeouts: both pools bound their statements, the
// application pool tightly since everything serving shares it.
func TestPoolStatementTimeouts(t *testing.T) {
	s := openStore(t)
	var app, owner string
	if err := s.app.QueryRow(t.Context(), `SHOW statement_timeout`).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if err := s.owner.QueryRow(t.Context(), `SHOW statement_timeout`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if app != "1min" || owner != "10min" {
		t.Fatalf("statement_timeout = %s on the application pool and %s on the owner's, want 1min and 10min", app, owner)
	}
}

func TestOpenRefusesUnsafeApplicationDSN(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	tests := []struct {
		name string
		url  string
		want error
	}{
		{"superuser as application role", testEnv(t, "KRITIKA_TEST_SUPER_URL"), ErrIsolationOff},
		{"owner as application role", testEnv(t, "KRITIKA_TEST_OWNER_URL"), ErrIsolationOff},
	}
	// The owner must own at least one table for the ownership check to bite.
	openStore(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(ctx, Options{AppURL: tt.url, Logger: logger})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Open = %v, want %v", err, tt.want)
			}
		})
	}
	t.Run("superuser as owner", func(t *testing.T) {
		_, err := Open(ctx, Options{AppURL: testEnv(t, "KRITIKA_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIKA_TEST_SUPER_URL"), Logger: logger})
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
	if err := s.Migrate(context.Background(), "kritika_app", "kritika_runner"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	names, _ := fs.Glob(migrationFS, "migrations/*.sql")
	var n int
	if err := s.owner.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(names) {
		t.Fatalf("schema_migrations rows = %d, err %v; want one per embedded migration (%d)", n, err, len(names))
	}
}

const alphaEntry = `
repositories:
  alpha/one: {}
  alpha/two: {}
`

// twoAccounts serves alpha and beta; alphaAccount, alpha alone.
const (
	twoAccounts = `
apps:
  alpha-bot:
    accounts: [alpha]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
  beta-bot:
    accounts: [beta]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
` + alphaEntry
	alphaAccount = `
apps:
  alpha-bot:
    accounts: [alpha]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
` + alphaEntry
)

func parse(t *testing.T, yaml string) *configfile.File {
	t.Helper()
	t.Setenv("KRITIKA_TEST_TOKEN", "tok")
	return configfiletest.Load(t, yaml)
}

func accountID(t *testing.T, s *Store, name string) string {
	t.Helper()
	var id string
	if err := s.owner.QueryRow(context.Background(), `SELECT id FROM accounts WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("account %s: %v", name, err)
	}
	return id
}

func TestApplyConfigAndRowLevelSecurity(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := parse(t, twoAccounts)
	if err := s.ApplyConfig(ctx, f); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if got, _ := s.AppliedConfigHash(ctx); got != f.Hash() {
		t.Fatalf("applied hash = %q, want %q", got, f.Hash())
	}
	alpha, beta := accountID(t, s, "alpha"), accountID(t, s, "beta")

	count := func(t *testing.T, account, table string) int {
		t.Helper()
		var n int
		err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		if err != nil {
			t.Fatalf("count %s as %s: %v", table, account, err)
		}
		return n
	}

	t.Run("each account sees only its own rows", func(t *testing.T) {
		if count(t, alpha, "accounts") != 1 || count(t, alpha, "repositories") != 2 {
			t.Fatal("alpha should see its own account and two repositories")
		}
		if count(t, beta, "repositories") != 0 || count(t, beta, "connections WHERE name IN ('alpha-bot', 'beta-bot')") != 2 {
			t.Fatal("beta should see no repositories, and every connection, which belong to no account")
		}
	})

	t.Run("no account set sees nothing", func(t *testing.T) {
		for _, table := range []string{"accounts", "repositories", "model_leases"} {
			var n int
			if err := s.app.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("%s: %d rows visible with no account set", table, n)
			}
		}
	})

	t.Run("account does not leak across pooled connections", func(t *testing.T) {
		// Exhaust the pool's connections through WithAccount, then query bare.
		for range 20 {
			if count(t, alpha, "repositories") != 2 {
				t.Fatal("alpha count changed")
			}
		}
		var n int
		if err := s.app.QueryRow(ctx, `SELECT count(*) FROM repositories`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("bare query after account transactions saw %d rows (err %v)", n, err)
		}
	})

	t.Run("application role cannot insert into another account", func(t *testing.T) {
		err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO model_leases (account_id, model_key, slot) VALUES ($1, 'p/m', 0)`, beta)
			return err
		})
		if err == nil {
			t.Fatal("insert with a foreign account_id must fail the WITH CHECK policy")
		}
	})

	t.Run("a listed repository is recorded enabled whatever its account starts it as", func(t *testing.T) {
		off := strings.Replace(twoAccounts, "    name: alpha\n", "    name: alpha\n    enabled: false\n", 1)
		if err := s.ApplyConfig(ctx, parse(t, off)); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		t.Cleanup(func() {
			if err := s.ApplyConfig(context.Background(), parse(t, twoAccounts)); err != nil {
				t.Errorf("ApplyConfig: %v", err)
			}
		})
		var enabled bool
		var disabledAt *time.Time
		if err := s.owner.QueryRow(ctx, `SELECT enabled, disabled_at FROM repositories WHERE name = 'alpha/two'`).Scan(&enabled, &disabledAt); err != nil {
			t.Fatal(err)
		}
		if !enabled || disabledAt != nil {
			t.Fatalf("alpha/two enabled=%v disabled_at=%v; want it recorded as listed", enabled, disabledAt)
		}
	})

	t.Run("an account no connection serves any more is disabled and keeps its rows", func(t *testing.T) {
		f2 := parse(t, alphaAccount)
		if err := s.ApplyConfig(ctx, f2); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		var enabled bool
		if err := s.owner.QueryRow(ctx, `SELECT enabled FROM accounts WHERE name = 'beta'`).Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled {
			t.Fatal("beta is still enabled")
		}
		var instEnabled bool
		if err := s.owner.QueryRow(ctx, `SELECT enabled FROM connections WHERE name = 'beta-bot'`).Scan(&instEnabled); err != nil || instEnabled {
			t.Fatalf("beta-bot enabled=%v err=%v; want disabled", instEnabled, err)
		}
		if err := s.ApplyConfig(ctx, f); err != nil {
			t.Fatalf("re-apply: %v", err)
		}
		if err := s.owner.QueryRow(ctx, `SELECT enabled FROM accounts WHERE name = 'beta'`).Scan(&enabled); err != nil || !enabled {
			t.Fatalf("beta re-enabled=%v err=%v", enabled, err)
		}
	})
}

// TestApplyConfigHandsUnlistedRepositoryBack: a repository its account no
// longer lists goes back to the forge, enabled, and listing it again takes
// it over.
func TestApplyConfigHandsUnlistedRepositoryBack(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	listed := parse(t, twoAccounts)
	if err := s.ApplyConfig(ctx, listed); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	unlisted := parse(t, strings.Replace(twoAccounts, "  alpha/two: {}\n", "", 1))
	check := func(wantOrigin string, wantEnabled bool) {
		t.Helper()
		var origin string
		var enabled bool
		var disabledAt *time.Time
		if err := s.owner.QueryRow(ctx, `SELECT managed_by, enabled, disabled_at FROM repositories WHERE name = 'alpha/two'`).
			Scan(&origin, &enabled, &disabledAt); err != nil {
			t.Fatal(err)
		}
		if origin != wantOrigin || enabled != wantEnabled || (disabledAt == nil) != wantEnabled {
			t.Fatalf("alpha/two managed_by=%s enabled=%v disabled_at=%v; want %s, enabled %v", origin, enabled, disabledAt, wantOrigin, wantEnabled)
		}
	}
	if err := s.ApplyConfig(ctx, unlisted); err != nil {
		t.Fatalf("ApplyConfig unlisted: %v", err)
	}
	check("forge", true)
	if err := s.ApplyConfig(ctx, listed); err != nil {
		t.Fatalf("ApplyConfig listed: %v", err)
	}
	check("file", true)
}

// TestRepositoryNamesIgnoreCase: GitHub's spelling of a listed repository
// names the same row, which takes that spelling, stays listed through the
// next apply, and is found in any case.
func TestRepositoryNamesIgnoreCase(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := parse(t, twoAccounts)
	if err := s.ApplyConfig(ctx, f); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := accountID(t, s, "alpha")
	spell := func(name string) (id string, isNew bool) {
		t.Helper()
		if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
			var err error
			id, isNew, err = EnsureRepository(ctx, tx, alpha, ReachedRepository{FullName: name})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id, isNew
	}
	// The suites share the database, and later ones look the row up as
	// alpha/one.
	t.Cleanup(func() { spell("alpha/one") })
	id, isNew := spell("Alpha/ONE")
	if isNew || id != configfile.RepositoryID(alpha, "alpha/one") {
		t.Fatalf("EnsureRepository = %s, new %v; want the listed row", id, isNew)
	}
	if err := s.ApplyConfig(ctx, f); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	var name, origin string
	var rows int
	if err := s.owner.QueryRow(ctx, `SELECT name, managed_by, (SELECT count(*) FROM repositories WHERE account_id = $1)
		FROM repositories WHERE id = $2`, alpha, id).Scan(&name, &origin, &rows); err != nil {
		t.Fatal(err)
	}
	if name != "Alpha/ONE" || origin != "file" || rows != 2 {
		t.Fatalf("name %q, managed_by %s, %d rows; want GitHub's spelling on the one listed row", name, origin, rows)
	}
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		row, err := FindRepo(ctx, tx, "alpha/one")
		if err == nil && row.ID != id {
			err = fmt.Errorf("found %s", row.ID)
		}
		return err
	}); err != nil {
		t.Fatalf("FindRepo in another case: %v", err)
	}
}

// TestRepositoryTraits: a report that says whether a repository is
// archived or a fork records it, and one that does not, as an installation
// event, keeps what was known.
func TestRepositoryTraits(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := accountID(t, s, "alpha")
	t.Cleanup(func() {
		_, _ = s.owner.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, configfile.RepositoryID(alpha, "alpha/copy"))
	})
	report := func(traits *configfile.RepoTraits) configfile.RepoTraits {
		t.Helper()
		var row RepoRow
		if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
			if _, _, err := EnsureRepository(ctx, tx, alpha, ReachedRepository{FullName: "alpha/copy", Traits: traits}); err != nil {
				return err
			}
			var err error
			row, err = FindRepo(ctx, tx, "alpha/copy")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return row.RepoTraits
	}
	for _, step := range []struct {
		name   string
		report *configfile.RepoTraits
		want   configfile.RepoTraits
	}{
		{"first named without traits", nil, configfile.RepoTraits{}},
		{"reported archived fork", &configfile.RepoTraits{Archived: true, Fork: true}, configfile.RepoTraits{Archived: true, Fork: true}},
		{"named again without traits", nil, configfile.RepoTraits{Archived: true, Fork: true}},
		{"reported unarchived", &configfile.RepoTraits{Fork: true}, configfile.RepoTraits{Fork: true}},
	} {
		if got := report(step.report); got != step.want {
			t.Fatalf("%s: traits %+v, want %+v", step.name, got, step.want)
		}
	}
}

func TestRunnerRoleUpdatesOnlyWhatARunnerReports(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := accountID(t, s, "alpha"), accountID(t, s, "beta")
	var runID string
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, kind) VALUES ($1, 'index') RETURNING id`, alpha).Scan(&runID)
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := Open(ctx, Options{AppURL: testEnv(t, "KRITIKA_TEST_RUNNER_URL"), Logger: slog.New(slog.DiscardHandler)})
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
		{name: "account", stmt: `UPDATE runner_runs SET account_id = '` + beta + `' WHERE id = $1`},
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
	var account string
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT account_id FROM runner_runs WHERE id = $1`, runID).Scan(&account)
	}); err != nil || account != alpha {
		t.Fatalf("run account = %q, %v", account, err)
	}
	// What a runner writes carries its run's account: a pack under another
	// account is refused by the policy, not left invisible to every reader.
	for _, tt := range []struct {
		name    string
		account string
		allowed bool
	}{{"own account", alpha, true}, {"another account", beta, false}} {
		t.Run("index pack under "+tt.name, func(t *testing.T) {
			err := runner.WithRunnerJob(ctx, runID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO index_packs (runner_run_id, account_id, mode) VALUES ($1, $2, 'full')`, runID, tt.account)
				return err
			})
			var pgErr *pgconn.PgError
			switch {
			case tt.allowed && err != nil:
				t.Fatalf("a runner must be able to write its pack: %v", err)
			case !tt.allowed && (!errors.As(err, &pgErr) || pgErr.Code != "42501"):
				t.Fatalf("a pack under another account must be refused, got %v", err)
			}
		})
	}
}

func TestRunSecretsToSweep(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := accountID(t, s, "alpha"), accountID(t, s, "beta")
	insert := func(account, age string, finished, swept bool) string {
		t.Helper()
		var id string
		if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, kind, created_at, finished_at, secret_swept_at)
				VALUES ($1, 'index', now() - $2::interval,
					CASE WHEN $3 THEN now() END, CASE WHEN $4 THEN now() END) RETURNING id`,
				account, age, finished, swept).Scan(&id)
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
	// Marking is idempotent and scoped to the account: beta's run is not
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
	if super := os.Getenv("KRITIKA_TEST_SUPER_URL"); super != "" {
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, super)
		if err == nil {
			_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;
				GRANT ALL ON SCHEMA public TO kritika; GRANT USAGE ON SCHEMA public TO kritika_app, kritika_runner;
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
	if err := s.Migrate(ctx, "kritika_app", "kritika_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// The suites share one database: leave no index schema behind.
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `DROP TABLE IF EXISTS index_chunks; DELETE FROM index_schema`)
	})
	if _, err := s.EnsureIndexSchema(ctx, "kritika_app", "test-embed", 8); err != nil {
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

// TestEnsureIndexSchemaRebuildsForANewEmbedder: the same embedder leaves
// the index alone, and another model or dimension drops every generation
// with the table and leaves each repository to onboard afresh.
func TestEnsureIndexSchemaRebuildsForANewEmbedder(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `UPDATE repositories SET active_index_run_id = NULL;
			DROP TABLE IF EXISTS index_chunks; DELETE FROM index_schema`)
	})
	ensure := func(model string, dims int) bool {
		t.Helper()
		rebuilt, err := s.EnsureIndexSchema(ctx, "kritika_app", model, dims)
		if err != nil {
			t.Fatalf("EnsureIndexSchema(%s, %d): %v", model, dims, err)
		}
		return rebuilt
	}
	if ensure("test-embed", 8) {
		t.Fatal("creating the index is not a rebuild")
	}
	var run string
	if err := s.owner.QueryRow(ctx, `WITH g AS (
			INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
			SELECT account_id, id, 'abc', 'test-embed', 8, 'full', 'completed' FROM repositories WHERE name = 'alpha/one'
			RETURNING id, account_id, repository_id
		), c AS (
			INSERT INTO index_chunks (account_id, repository_id, index_run_id, path, start_line, end_line, text, embedding)
			SELECT account_id, repository_id, id, 'main.go', 1, 1, 'package main', '[1,0,0,0,0,0,0,0]' FROM g
		)
		UPDATE repositories r SET active_index_run_id = g.id FROM g WHERE r.id = g.repository_id RETURNING g.id`).Scan(&run); err != nil {
		t.Fatalf("seed a generation: %v", err)
	}
	if ensure("test-embed", 8) {
		t.Fatal("the same embedder must not rebuild the index")
	}
	for _, next := range []struct {
		model string
		dims  int
	}{{"other-embed", 8}, {"other-embed", 16}} {
		if !ensure(next.model, next.dims) {
			t.Fatalf("%s/%d must rebuild the index", next.model, next.dims)
		}
		var active *string
		var status string
		if err := s.owner.QueryRow(ctx, `SELECT r.active_index_run_id::text, g.status FROM repositories r, index_runs g
			WHERE r.name = 'alpha/one' AND g.id = $1`, run).Scan(&active, &status); err != nil {
			t.Fatal(err)
		}
		if active != nil || status != "superseded" {
			t.Fatalf("after the rebuild: active = %v, status = %s", active, status)
		}
		var model string
		var dims int
		if err := s.owner.QueryRow(ctx, `SELECT embed_model, embed_dims FROM index_schema`).Scan(&model, &dims); err != nil ||
			model != next.model || dims != next.dims {
			t.Fatalf("index_schema = %s/%d, %v", model, dims, err)
		}
		var typmod string
		if err := s.owner.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute
			WHERE attrelid = 'index_chunks'::regclass AND attname = 'embedding'`).Scan(&typmod); err != nil ||
			typmod != fmt.Sprintf("halfvec(%d)", next.dims) {
			t.Fatalf("embedding column = %s, %v", typmod, err)
		}
	}
}

// TestSweepDisabledIndexes checks that only a repository disabled for longer
// than the grace loses its index, that it is left to onboard afresh, and
// that a second sweep finds nothing.
func TestSweepDisabledIndexes(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	// The suites share one database: leave no index schema behind.
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `UPDATE repositories SET active_index_run_id = NULL;
			DROP TABLE IF EXISTS index_chunks; DELETE FROM index_schema`)
	})
	if _, err := s.EnsureIndexSchema(ctx, "kritika_app", "test-embed", 8); err != nil {
		t.Fatalf("EnsureIndexSchema: %v", err)
	}
	// Both alpha repositories get an active generation with one chunk;
	// an admin turns alpha/two off, and the App loses alpha/one later.
	runs := map[string]string{}
	for _, name := range []string{"alpha/one", "alpha/two"} {
		var run string
		if err := s.owner.QueryRow(ctx, `WITH g AS (
				INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
				SELECT account_id, id, 'abc', 'test-embed', 8, 'full', 'completed' FROM repositories WHERE name = $1
				RETURNING id, account_id, repository_id
			), c AS (
				INSERT INTO index_chunks (account_id, repository_id, index_run_id, path, start_line, end_line, text, embedding)
				SELECT account_id, repository_id, id, 'main.go', 1, 1, 'package main', '[1,0,0,0,0,0,0,0]' FROM g
			)
			UPDATE repositories r SET active_index_run_id = g.id FROM g WHERE r.id = g.repository_id RETURNING g.id`, name).Scan(&run); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		runs[name] = run
	}
	state := func(name string) (active bool, status string, chunks int) {
		t.Helper()
		if err := s.owner.QueryRow(ctx, `SELECT
				(SELECT active_index_run_id IS NOT NULL FROM repositories WHERE name = $1),
				(SELECT status FROM index_runs WHERE id = $2),
				(SELECT count(*) FROM index_chunks WHERE index_run_id = $2)`, name, runs[name]).Scan(&active, &status, &chunks); err != nil {
			t.Fatal(err)
		}
		return active, status, chunks
	}
	sweep := func(want int64) {
		t.Helper()
		if n, err := s.SweepDisabledIndexes(ctx, time.Hour); err != nil || n != want {
			t.Fatalf("SweepDisabledIndexes = %d, %v; want %d", n, err, want)
		}
	}

	exec := func(sql string) {
		t.Helper()
		if _, err := s.owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `UPDATE repositories SET turned_on = NULL, turned_at = NULL, enabled = true, disabled_at = NULL
			WHERE name IN ('alpha/one', 'alpha/two')`)
	})
	alpha := accountID(t, s, "alpha")
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		return TurnOn(ctx, tx, configfile.RepositoryID(alpha, "alpha/two"), false)
	}); err != nil {
		t.Fatal(err)
	}

	sweep(0)
	if active, status, chunks := state("alpha/two"); !active || status != "completed" || chunks != 1 {
		t.Fatalf("within the grace: active=%v status=%s chunks=%d, want the index kept", active, status, chunks)
	}
	exec(`UPDATE repositories SET turned_at = now() - interval '2 hours' WHERE name = 'alpha/two'`)
	sweep(1)
	if active, status, chunks := state("alpha/two"); active || status != "superseded" || chunks != 0 {
		t.Fatalf("turned off past the grace: active=%v status=%s chunks=%d, want the index dropped", active, status, chunks)
	}
	if active, status, chunks := state("alpha/one"); !active || status != "completed" || chunks != 1 {
		t.Fatalf("a repository that runs: active=%v status=%s chunks=%d, want the index kept", active, status, chunks)
	}
	sweep(0)
	exec(`UPDATE repositories SET enabled = false, disabled_at = now() - interval '2 hours' WHERE name = 'alpha/one'`)
	sweep(1)
	if active, status, chunks := state("alpha/one"); active || status != "superseded" || chunks != 0 {
		t.Fatalf("lost past the grace: active=%v status=%s chunks=%d, want the index dropped", active, status, chunks)
	}
}

// TestFindRepo: a repository is found by its full name within the account
// the transaction reads.
func TestFindRepo(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	find := func(account, name string) (RepoRow, error) {
		t.Helper()
		var row RepoRow
		err := s.WithAccount(ctx, accountID(t, s, account), func(tx pgx.Tx) error {
			var err error
			row, err = FindRepo(ctx, tx, name)
			return err
		})
		return row, err
	}
	if row, err := find("alpha", "alpha/one"); err != nil || row.FullName != "alpha/one" || !row.Enabled || row.ManagedBy != "file" {
		t.Fatalf("FindRepo(alpha/one) = %+v, %v", row, err)
	}
	if _, err := find("beta", "alpha/one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindRepo from another account = %v, want ErrNotFound", err)
	}
}

// changeLast is s with its last character changed to one it cannot
// already be.
func changeLast(s string) string {
	if strings.HasSuffix(s, "0") {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}

func TestGatewayTokens(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := accountID(t, s, "alpha")
	// A run needs a review, which needs a pull request of a repository.
	grant := func() GatewayGrant {
		t.Helper()
		g := GatewayGrant{AccountID: alpha, Model: "openrouter/acme/large", Fallback: "openrouter/acme/small", Budget: 1000}
		if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE name = 'alpha/one'`).Scan(&g.RepositoryID); err != nil {
				return err
			}
			var prID string
			if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, head_sha)
				VALUES ($1, $2, (random() * 1e6)::int, 'abc') RETURNING id`, alpha, g.RepositoryID).Scan(&prID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status)
				VALUES ($1, $2, 'abc', 'running') RETURNING id`, alpha, prID).Scan(&g.ReviewID); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, kind) VALUES ($1, 'review') RETURNING id`, alpha).Scan(&g.RunID)
		}); err != nil {
			t.Fatal(err)
		}
		return g
	}

	g := grant()
	token, err := s.MintGatewayToken(ctx, g, time.Now().Add(time.Hour))
	if err != nil || !strings.HasPrefix(token, "krk_") || len(token) != 4+64 {
		t.Fatalf("token = %q, %v", token, err)
	}
	var stored int
	if err := s.owner.QueryRow(ctx, `SELECT count(*) FROM gateway_tokens WHERE token_hash = convert_to($1, 'UTF8')`, token).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the token itself is stored: %d, %v", stored, err)
	}
	got, err := s.LookupGatewayToken(ctx, token)
	if err != nil || got != g {
		t.Fatalf("grant = %+v, %v; want %+v", got, err, g)
	}
	for range 2 {
		if err := s.ChargeGatewayToken(ctx, token, 300); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.LookupGatewayToken(ctx, token); err != nil || got.Spent != 600 {
		t.Fatalf("spent = %d, %v", got.Spent, err)
	}
	checkReservations(t, s, token)
	for _, bad := range []string{"", "krk_", changeLast(token), "sk-" + token[4:]} {
		if _, err := s.LookupGatewayToken(ctx, bad); !errors.Is(err, ErrGatewayToken) {
			t.Fatalf("lookup %q = %v, want ErrGatewayToken", bad, err)
		}
	}

	expired, err := s.MintGatewayToken(ctx, grant(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupGatewayToken(ctx, expired); !errors.Is(err, ErrGatewayToken) {
		t.Fatalf("expired token = %v", err)
	}
	if ok, err := s.ReserveGatewayTokens(ctx, expired, 1); err != nil || ok {
		t.Fatalf("reserve on an expired token = %v, %v", ok, err)
	}
	other := grant()
	live, err := s.MintGatewayToken(ctx, other, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Revoking a run takes its tokens and the expired ones, not another
	// run's live token.
	if err := s.RevokeGatewayTokens(ctx, g.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupGatewayToken(ctx, token); !errors.Is(err, ErrGatewayToken) {
		t.Fatalf("revoked token = %v", err)
	}
	var left int
	if err := s.owner.QueryRow(ctx, `SELECT count(*) FROM gateway_tokens`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("tokens left = %d, %v; want the other run's", left, err)
	}
	if _, err := s.LookupGatewayToken(ctx, live); err != nil {
		t.Fatalf("other run's token = %v", err)
	}
	// The suites share one database: leave no live token behind.
	if err := s.RevokeGatewayTokens(ctx, other.RunID); err != nil {
		t.Fatal(err)
	}
}

// checkReservations reserves against a token with 600 of its 1000 tokens
// spent: a reservation succeeds while the run is under its budget, whatever
// its size, and not once the run is at or over it; a refund brings it back.
func checkReservations(t *testing.T, s *Store, token string) {
	t.Helper()
	ctx := context.Background()
	for _, tt := range []struct {
		tokens int64
		ok     bool
		spent  int64
	}{{300, true, 900}, {5000, true, 5900}, {1, false, 5900}} {
		ok, err := s.ReserveGatewayTokens(ctx, token, tt.tokens)
		if err != nil || ok != tt.ok {
			t.Fatalf("reserve %d = %v, %v; want %v", tt.tokens, ok, err, tt.ok)
		}
		if got, _ := s.LookupGatewayToken(ctx, token); got.Spent != tt.spent {
			t.Fatalf("spent after reserving %d = %d, want %d", tt.tokens, got.Spent, tt.spent)
		}
	}
	if err := s.ChargeGatewayToken(ctx, token, -5000); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReserveGatewayTokens(ctx, token, 1); err != nil || !ok {
		t.Fatalf("reserve after a refund = %v, %v", ok, err)
	}
}
