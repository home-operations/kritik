// Package store owns kritik's Postgres access: the two connection pools,
// the startup assertion that keeps row-level security honest, schema
// migrations, the leader lock, tenant-scoped transactions, and the sync of
// the configuration file into file-managed rows.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store holds the application pool every request and job uses and, on
// leader-eligible roles, the owner pool that runs migrations and the
// configuration sync.
type Store struct {
	app    *pgxpool.Pool
	owner  *pgxpool.Pool
	logger *slog.Logger
}

// newPool opens a pool with server-side TCP keepalives, so Postgres drops
// the session of a client that died without closing it (a node lost, a pod
// killed) within about a minute, and with it any advisory lock the session
// held. Postgres's own defaults leave that to the kernel's two hours. A
// statement timeout, when given, bounds every statement on the pool.
func newPool(ctx context.Context, url, application string, statementTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	params := cfg.ConnConfig.RuntimeParams
	params["application_name"] = application
	params["tcp_keepalives_idle"] = "30"
	params["tcp_keepalives_interval"] = "10"
	params["tcp_keepalives_count"] = "3"
	if statementTimeout > 0 {
		params["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Options configure Open.
type Options struct {
	// AppURL is the application role's DSN. Required.
	AppURL string
	// OwnerURL is the owner role's DSN. Optional; without it the process can
	// never become leader.
	OwnerURL string
	// AppRole and RunnerRole are the role names migrations grant to.
	AppRole    string
	RunnerRole string
	Logger     *slog.Logger
}

// Open connects both pools and runs the startup assertions. It fails, rather
// than serving, when the application DSN would bypass row-level security or
// the vector extension is missing, because either would be invisible at
// runtime and wrong.
func Open(ctx context.Context, opts Options) (*Store, error) {
	app, err := newPool(ctx, opts.AppURL, "kritik-app", 0)
	if err != nil {
		return nil, fmt.Errorf("store: application pool: %w", err)
	}
	s := &Store{app: app, logger: opts.Logger}
	if err := s.assertApplicationRole(ctx); err != nil {
		app.Close()
		return nil, err
	}
	if err := s.assertExtension(ctx); err != nil {
		app.Close()
		return nil, err
	}
	if opts.OwnerURL != "" {
		// Migrations may build an index for minutes; anything longer on the
		// owner connection is a hang worth breaking.
		owner, err := newPool(ctx, opts.OwnerURL, "kritik-owner", 10*time.Minute)
		if err != nil {
			app.Close()
			return nil, fmt.Errorf("store: owner pool: %w", err)
		}
		if err := assertOwnerRole(ctx, owner); err != nil {
			app.Close()
			owner.Close()
			return nil, err
		}
		s.owner = owner
	}
	return s, nil
}

// Close releases both pools.
func (s *Store) Close() {
	s.app.Close()
	if s.owner != nil {
		s.owner.Close()
	}
}

// App exposes the application pool for tenant-scoped work; prefer
// [Store.WithTenant].
func (s *Store) App() *pgxpool.Pool { return s.app }

// LeaderEligible reports whether an owner DSN was configured.
func (s *Store) LeaderEligible() bool { return s.owner != nil }

// ErrIsolationOff is returned when the application DSN's role could bypass
// row-level security.
var ErrIsolationOff = errors.New("store: application role would bypass row-level security")

// assertApplicationRole refuses a DSN whose role is a superuser, has
// BYPASSRLS, or owns any table in the schema. Any one of those silently
// disables every policy.
func (s *Store) assertApplicationRole(ctx context.Context) error {
	var super, bypass bool
	var owned int
	err := s.app.QueryRow(ctx, `
		SELECT r.rolsuper, r.rolbypassrls,
		       (SELECT count(*) FROM pg_tables t WHERE t.schemaname = current_schema() AND t.tableowner = r.rolname)
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&super, &bypass, &owned)
	if err != nil {
		return fmt.Errorf("store: inspect application role: %w", err)
	}
	switch {
	case super:
		return fmt.Errorf("%w: role is a superuser", ErrIsolationOff)
	case bypass:
		return fmt.Errorf("%w: role has BYPASSRLS", ErrIsolationOff)
	case owned > 0:
		return fmt.Errorf("%w: role owns %d tables in the schema", ErrIsolationOff, owned)
	}
	return nil
}

// ErrOwnerSuperuser is returned when the owner DSN's role is a superuser.
var ErrOwnerSuperuser = errors.New("store: owner role must not be a superuser")

// assertOwnerRole refuses a superuser as owner: the owner is meant to own the
// tables and nothing more, and the extension is the operator's job.
func assertOwnerRole(ctx context.Context, owner *pgxpool.Pool) error {
	var super bool
	if err := owner.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		return fmt.Errorf("store: inspect owner role: %w", err)
	}
	if super {
		return ErrOwnerSuperuser
	}
	return nil
}

// IsConfigurationError reports whether err is one of the startup assertions,
// which no amount of retrying will fix, as opposed to a database that is
// not reachable yet.
func IsConfigurationError(err error) bool {
	return errors.Is(err, ErrIsolationOff) || errors.Is(err, ErrNoVectorExtension) || errors.Is(err, ErrOwnerSuperuser)
}

// ErrNoVectorExtension is returned when VectorChord (vchord) or pgvector
// (vector, whose types it builds on) is absent.
var ErrNoVectorExtension = errors.New("store: the vchord and vector extensions are not both installed in this database")

func (s *Store) assertExtension(ctx context.Context) error {
	var missing []string
	if err := s.app.QueryRow(ctx, `SELECT array_agg(name ORDER BY name) FROM unnest(ARRAY['vchord', 'vector']) AS name
		WHERE NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = name)`).Scan(&missing); err != nil {
		return fmt.Errorf("store: inspect extensions: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w (missing: %s); use a VectorChord image with vchord in shared_preload_libraries, and on CloudNativePG "+
			"declare vector and vchord on the Database resource, elsewhere CREATE EXTENSION vchord CASCADE as a superuser",
			ErrNoVectorExtension, strings.Join(missing, ", "))
	}
	return nil
}

// WithRunnerJob runs fn in a transaction with the runner job set
// transaction-locally, so the runner_job policies open exactly that run's
// rows. Used by the runner role, whose DSN is the runner role's.
func (s *Store) WithRunnerJob(ctx context.Context, runID string, fn func(pgx.Tx) error) error {
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.runner_job_id', $1, true)`, runID); err != nil {
		return fmt.Errorf("store: set runner job: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// WithTenant runs fn in a transaction on the application pool with the
// tenant set transaction-locally, so every policy resolves to that tenant
// and nothing survives on the pooled connection after commit or rollback.
func (s *Store) WithTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("store: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
