// Package store owns kritika's Postgres access: the two connection pools,
// the startup assertion that keeps row-level security honest, schema
// migrations, the leader lock, account-scoped transactions, and applying
// the running configuration, the file's and the dashboard's, to the rows
// it declares.
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

// Pool ceilings when the URI sets no pool_max_conns. pgx's own default is
// the node's CPU count, and a serve pod has no CPU limit, so on a large
// node two replicas alone could take most of Postgres's 100 connections.
// The application pool carries River's fetchers, a handful of jobs and the
// requests runner pods and the dashboard make; the owner pool only the
// leader's duties. Connections open on demand, so a ceiling costs an idle
// process nothing.
const (
	appPoolMaxConns   = 16
	ownerPoolMaxConns = 4
)

// appStatementTimeout bounds every statement on the application pool. The
// pool is shared by River, webhook ingest, the gateway and the dashboard,
// so a dashboard query over a large account that ran unbounded could hold
// its connections and stall the rest; nothing the pool runs is meant to
// take anywhere near this long.
const appStatementTimeout = time.Minute

// pingTimeout bounds the ping a pool gives an idle connection before
// handing it out. Without it the ping waits on the kernel's TCP
// retransmissions, about fifteen minutes, when the server died without
// closing the socket: every acquire on the pool hangs that long, the
// leader's lock attempt included. A ping is an empty query, so a few
// seconds is generous for a server that is up.
const pingTimeout = 5 * time.Second

// newPool opens a pool with server-side TCP keepalives, so Postgres drops
// the session of a client that died without closing it (a node lost, a pod
// killed) within about a minute, and with it any advisory lock the session
// held. Postgres's own defaults leave that to the kernel's two hours. The
// pool's own ping of an idle connection is bounded by pingTimeout, so a
// server that died the same way costs a client seconds, not the kernel's
// retransmissions. A statement timeout, when given, bounds every statement
// on the pool.
func newPool(ctx context.Context, url, application string, statementTimeout time.Duration, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(url, application, statementTimeout, maxConns)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// poolConfig is newPool's configuration. maxConns is the pool's ceiling
// and pingTimeout its ping bound unless the URI names its own with
// pool_max_conns or pool_ping_timeout, which pgx has already read by the
// time the config is parsed.
func poolConfig(url, application string, statementTimeout time.Duration, maxConns int32) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = maxConns
	}
	if !strings.Contains(url, "pool_ping_timeout") {
		cfg.PingTimeout = pingTimeout
	}
	params := cfg.ConnConfig.RuntimeParams
	params["application_name"] = application
	params["tcp_keepalives_idle"] = "30"
	params["tcp_keepalives_interval"] = "10"
	params["tcp_keepalives_count"] = "3"
	if statementTimeout > 0 {
		params["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	}
	return cfg, nil
}

// Options configure Open.
type Options struct {
	// AppURL is the application role's DSN. Required.
	AppURL string
	// OwnerURL is the owner role's DSN. Optional; without it the process can
	// never become leader.
	OwnerURL string
	Logger   *slog.Logger
}

// Open connects both pools and runs the startup assertions. It fails, rather
// than serving, when the application DSN would bypass row-level security or
// the vector extension is missing, because either would be invisible at
// runtime and wrong.
func Open(ctx context.Context, opts Options) (*Store, error) {
	app, err := newPool(ctx, opts.AppURL, "kritika-app", appStatementTimeout, appPoolMaxConns)
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
		owner, err := newPool(ctx, opts.OwnerURL, "kritika-owner", 10*time.Minute, ownerPoolMaxConns)
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

// App exposes the application pool for account-scoped work; prefer
// [Store.WithAccount].
func (s *Store) App() *pgxpool.Pool { return s.app }

// LeaderEligible reports whether an owner DSN was configured.
func (s *Store) LeaderEligible() bool { return s.owner != nil }

// PoolStats is each pool's live state by name, app and, when configured,
// owner, for the metrics.
func (s *Store) PoolStats() map[string]func() *pgxpool.Stat {
	stats := map[string]func() *pgxpool.Stat{"app": s.app.Stat}
	if s.owner != nil {
		stats["owner"] = s.owner.Stat
	}
	return stats
}

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
// tables and nothing more, and the extension is the admin's job.
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
	return s.withSetting(ctx, "app.runner_job_id", runID, "store: set runner job", fn)
}

// WithAccount runs fn in a transaction on the application pool with the
// account set transaction-locally, so every policy resolves to that account
// and nothing survives on the pooled connection after commit or rollback.
func (s *Store) WithAccount(ctx context.Context, accountID string, fn func(pgx.Tx) error) error {
	return s.withSetting(ctx, "app.account_id", accountID, "store: set account", fn)
}

// withSetting runs fn in a transaction on the application pool with the
// setting key set to value transaction-locally; setErr prefixes a failure
// to set it.
func (s *Store) withSetting(ctx context.Context, key, value, setErr string, fn func(pgx.Tx) error) error {
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, key, value); err != nil {
		return fmt.Errorf("%s: %w", setErr, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
