package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies River's schema and then kritik's own migrations, in order,
// each in its own transaction, recording each in schema_migrations. It runs
// as the owner and is idempotent. Grants to the application and runner
// roles are re-applied after every run because a new table needs them and
// GRANT is idempotent.
func (s *Store) Migrate(ctx context.Context, appRole, runnerRole string) error {
	if s.owner == nil {
		return fmt.Errorf("store: migrate needs the owner DSN")
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(s.owner), nil)
	if err != nil {
		return fmt.Errorf("store: river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("store: river migrations: %w", err)
	}

	if _, err := s.owner.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("store: schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		if err := s.applyMigration(ctx, name, version); err != nil {
			return err
		}
	}
	return s.grant(ctx, appRole, runnerRole)
}

func (s *Store) applyMigration(ctx context.Context, name, version string) error {
	tx, err := s.owner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialise concurrent migrators; the leader lock already does, but a
	// manual run from a shell must not race it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kritik-migrate'))`); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
		return fmt.Errorf("store: check migration %s: %w", version, err)
	}
	if applied {
		return nil
	}
	sql, err := migrationFS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("store: read migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("store: record migration %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", version, err)
	}
	s.logger.Info("migration applied", "version", version)
	return nil
}

// SchemaReady reports whether every embedded migration has been applied, so
// a replica that is not the leader knows when it may start working jobs. A
// fresh database has no schema_migrations table at all, which is "not yet",
// not an error.
func (s *Store) SchemaReady(ctx context.Context) (bool, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return false, fmt.Errorf("store: list migrations: %w", err)
	}
	var exists bool
	if err := s.app.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: check schema: %w", err)
	}
	if !exists {
		return false, nil
	}
	var applied int
	if err := s.app.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		return false, fmt.Errorf("store: count migrations: %w", err)
	}
	return applied >= len(names), nil
}

// WaitForSchema polls SchemaReady until it is true or ctx ends.
func (s *Store) WaitForSchema(ctx context.Context, every time.Duration) error {
	for {
		ready, err := s.SchemaReady(ctx)
		if err != nil {
			s.logger.Warn("schema check failed, retrying", "error", err)
		} else if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// grant gives the application role what requests and jobs need and the
// runner role nothing yet beyond connecting; the runner's tables arrive with
// the runner. Role names come from configuration, so they are quoted as
// identifiers rather than interpolated raw.
func (s *Store) grant(ctx context.Context, appRole, runnerRole string) error {
	app := pgx.Identifier{appRole}.Sanitize()
	runner := pgx.Identifier{runnerRole}.Sanitize()
	stmts := []string{
		`GRANT USAGE ON SCHEMA public TO ` + app + `, ` + runner,
		`GRANT SELECT ON tenants, config_state, schema_migrations TO ` + app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON installations, repositories, model_leases, pull_requests TO ` + app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON reviews, runner_runs, context_packs, findings, sticky_comments, usage TO ` + app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON index_runs, index_packs, index_staging, followups, poll_state TO ` + app,
		`GRANT SELECT ON index_schema, agent_runs TO ` + app,
		// The runner role sees only its own job through the runner_job
		// policies; it needs the table privileges those policies gate.
		`GRANT SELECT, UPDATE ON runner_runs TO ` + runner,
		`GRANT SELECT, INSERT ON context_packs, index_packs, index_staging, agent_runs TO ` + runner,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO ` + runner,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO ` + app,
	}
	// River's tables are outside row-level security; the application role
	// inserts and works jobs. Their set changes across River versions, so
	// grant on whatever River's migrations created rather than a fixed list.
	rows, err := s.owner.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename LIKE 'river\_%'`)
	if err != nil {
		return fmt.Errorf("store: list river tables: %w", err)
	}
	river, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("store: list river tables: %w", err)
	}
	for _, table := range river {
		stmts = append(stmts, `GRANT SELECT, INSERT, UPDATE, DELETE ON `+pgx.Identifier{table}.Sanitize()+` TO `+app)
	}
	for _, stmt := range stmts {
		if _, err := s.owner.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: %s: %w", strings.SplitN(stmt, " TO ", 2)[0], err)
		}
	}
	return nil
}
