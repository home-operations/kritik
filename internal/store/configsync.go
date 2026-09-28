package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/home-operations/kritik/internal/configfile"
)

// ErrManagedBy is an ApplyConfig write that would take over a live row
// another origin manages. ApplyConfig disables every row whose origin no
// longer declares it before upserting, and merging the dashboard into the
// file leaves out a file account whose slug or name a dashboard account
// declares, so this guards the database rather than being expected.
var ErrManagedBy = errors.New("store: row is managed by another origin")

// IsConfigContentError reports whether an ApplyConfig error was caused by
// the configuration it was given rather than by the database: a row another
// origin holds, a value the schema cannot store (class 22), or a NOT NULL
// (23502) or CHECK (23514) constraint it breaks. Those depend only on the
// configuration. Unique (23505) and foreign-key (23503) violations are not
// counted: ingest inserting a repository concurrently with ApplyConfig
// raises one, and a retry succeeds.
func IsConfigContentError(err error) bool {
	if errors.Is(err, ErrManagedBy) {
		return true
	}
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "22") || pgErr.Code == "23502" || pgErr.Code == "23514"
}

// ApplyConfig upserts the file's accounts, connections and listed
// repositories as rows managed by each account's origin, disables file and
// dashboard rows the file no longer declares, and records the applied hash.
// A row declared again by another origin (a file account removed and a
// dashboard account of the same slug added, or the reverse) is disabled and
// then taken over; an enabled row is never taken over, and neither is a
// connection another account holds (both ErrManagedBy). It runs as the
// owner in one transaction, so a replica reading config_state never sees a
// half-applied file. Only the leader calls it.
func (s *Store) ApplyConfig(ctx context.Context, f *configfile.File, leader string) error {
	if s.owner == nil {
		return errors.New("store: applying configuration needs the owner DSN")
	}
	tx, err := s.owner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin config apply: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := disableUndeclared(ctx, tx, f); err != nil {
		return err
	}
	for i := range f.Accounts {
		t := &f.Accounts[i]
		accountID, err := upsertAccount(ctx, tx, t)
		if err != nil {
			return err
		}
		connectionIDs := map[string]string{}
		for j := range t.Connections {
			in := &t.Connections[j]
			id, err := upsertConnection(ctx, tx, accountID, t.Origin(), in)
			if err != nil {
				return err
			}
			connectionIDs[in.Name] = id
		}
		for j := range t.Repositories {
			r := &t.Repositories[j]
			in := f.ConnectionFor(t, r)
			if err := upsertRepository(ctx, tx, accountID, connectionIDs[in.Name], t.Origin(), r); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO config_state (id, applied_hash, applied_at, leader) VALUES (1, $1, now(), $2)
		ON CONFLICT (id) DO UPDATE SET applied_hash = EXCLUDED.applied_hash, applied_at = now(), leader = EXCLUDED.leader`,
		f.Hash(), leader); err != nil {
		return fmt.Errorf("store: record config state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit config apply: %w", err)
	}
	return nil
}

// disableUndeclared disables every file or dashboard account and connection
// f does not declare with that same origin, then every file or dashboard
// repository under a disabled account or connection or no longer listed by
// its account. The upserts that follow re-enable what f still declares.
// Forge-discovered repositories are left alone.
func disableUndeclared(ctx context.Context, tx pgx.Tx, f *configfile.File) error {
	slugs, slugOrigins := make([]string, 0, len(f.Accounts)), make([]string, 0, len(f.Accounts))
	var names, nameOrigins []string
	for i := range f.Accounts {
		t := &f.Accounts[i]
		slugs, slugOrigins = append(slugs, t.Slug), append(slugOrigins, string(t.Origin()))
		for j := range t.Connections {
			names, nameOrigins = append(names, t.Connections[j].Name), append(nameOrigins, string(t.Origin()))
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE managed_by IN ('file', 'dashboard') AND enabled
			AND (slug, managed_by) NOT IN (SELECT * FROM unnest($1::text[], $2::text[]))`, slugs, slugOrigins); err != nil {
		return fmt.Errorf("store: disable removed accounts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE connections SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE managed_by IN ('file', 'dashboard') AND enabled
			AND (name, managed_by) NOT IN (SELECT * FROM unnest($1::text[], $2::text[]))`, names, nameOrigins); err != nil {
		return fmt.Errorf("store: disable removed connections: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE repositories r SET enabled = false, disabled_at = coalesce(r.disabled_at, now()), updated_at = now()
		WHERE r.managed_by IN ('file', 'dashboard') AND r.enabled AND (
			EXISTS (SELECT 1 FROM accounts t WHERE t.id = r.account_id AND NOT t.enabled)
			OR EXISTS (SELECT 1 FROM connections i WHERE i.id = r.connection_id AND NOT i.enabled))`); err != nil {
		return fmt.Errorf("store: disable repositories of removed accounts and connections: %w", err)
	}
	for i := range f.Accounts {
		t := &f.Accounts[i]
		if _, err := tx.Exec(ctx, `
			UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
			WHERE account_id = $1 AND managed_by IN ('file', 'dashboard') AND enabled AND name <> ALL($2)`,
			t.ID(), repoNames(t)); err != nil {
			return fmt.Errorf("store: disable removed repositories: %w", err)
		}
	}
	return nil
}

// AppliedConfigHash returns the hash the leader last applied, or "" when no
// configuration has ever been applied. Any role may call it.
func (s *Store) AppliedConfigHash(ctx context.Context) (string, error) {
	var hash string
	err := s.app.QueryRow(ctx, `SELECT applied_hash FROM config_state WHERE id = 1`).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read config state: %w", err)
	}
	return hash, nil
}

func upsertAccount(ctx context.Context, tx pgx.Tx, t *configfile.Account) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO accounts (id, slug, managed_by) VALUES ($1, $2, $3)
		ON CONFLICT (slug) DO UPDATE SET
			managed_by = EXCLUDED.managed_by, enabled = true, disabled_at = NULL, updated_at = now()
		WHERE accounts.managed_by = EXCLUDED.managed_by OR NOT accounts.enabled
		RETURNING id`, t.ID(), t.Slug, string(t.Origin())).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", managedByConflict(ctx, tx, "account "+t.Slug, t.Origin(), `SELECT managed_by FROM accounts WHERE slug = $1`, t.Slug)
	}
	if err != nil {
		return "", fmt.Errorf("store: upsert account %s: %w", t.Slug, err)
	}
	return id, nil
}

func upsertConnection(
	ctx context.Context, tx pgx.Tx, accountID string, origin configfile.Origin, in *configfile.Connection,
) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO connections (id, account_id, name, forge, accounts, managed_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET
			account_id = EXCLUDED.account_id, forge = EXCLUDED.forge, accounts = EXCLUDED.accounts,
			managed_by = EXCLUDED.managed_by, enabled = true, disabled_at = NULL,
			updated_at = now()
		WHERE connections.account_id = EXCLUDED.account_id AND (connections.managed_by = EXCLUDED.managed_by OR NOT connections.enabled)
		RETURNING id`, in.ID(), accountID, in.Name, string(in.Forge), in.Accounts, string(origin)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another account's row keeps its history (pull requests, reviews,
		// repositories) under its own account id; handing the name over would
		// mix the two, so it stays with its account, enabled or not.
		return "", managedByConflict(ctx, tx, "connection "+in.Name, origin,
			`SELECT CASE WHEN account_id = $2 THEN managed_by ELSE 'another account' END FROM connections WHERE name = $1`,
			in.Name, accountID)
	}
	if err != nil {
		return "", fmt.Errorf("store: upsert connection %s: %w", in.Name, err)
	}
	return id, nil
}

// upsertRepository also takes over a forge-discovered row: listing a
// repository the poller or a webhook already found makes it declared.
func upsertRepository(
	ctx context.Context, tx pgx.Tx, accountID, connectionID string, origin configfile.Origin, r *configfile.Repository,
) error {
	enabled := r.Enabled == nil || *r.Enabled
	tag, err := tx.Exec(ctx, `
		INSERT INTO repositories (id, account_id, connection_id, name, managed_by, enabled, disabled_at)
		VALUES ($1, $2, $3, $4, $6, $5, CASE WHEN $5 THEN NULL ELSE now() END)
		ON CONFLICT (connection_id, name) DO UPDATE SET
			account_id = EXCLUDED.account_id, managed_by = EXCLUDED.managed_by, enabled = EXCLUDED.enabled,
			disabled_at = CASE WHEN EXCLUDED.enabled THEN NULL ELSE coalesce(repositories.disabled_at, now()) END,
			updated_at = now()
		WHERE repositories.managed_by IN (EXCLUDED.managed_by, 'forge') OR NOT repositories.enabled`,
		configfile.RepositoryID(connectionID, r.Name), accountID, connectionID, r.Name, enabled, string(origin))
	if err == nil && tag.RowsAffected() == 0 {
		return managedByConflict(ctx, tx, "repository "+r.Name, origin,
			`SELECT managed_by FROM repositories WHERE connection_id = $1 AND name = $2`, connectionID, r.Name)
	}
	if err != nil {
		return fmt.Errorf("store: upsert repository %s: %w", r.Name, err)
	}
	return nil
}

// managedByConflict is the ErrManagedBy for what, naming the origin that
// holds it, read with query.
func managedByConflict(ctx context.Context, tx pgx.Tx, what string, origin configfile.Origin, query string, args ...any) error {
	var holder string
	if err := tx.QueryRow(ctx, query, args...).Scan(&holder); err != nil {
		return fmt.Errorf("store: %s: %w (reading its owner: %w)", what, ErrManagedBy, err)
	}
	return fmt.Errorf("store: %s is managed by %s, not taking it over as %s: %w", what, holder, origin, ErrManagedBy)
}

func repoNames(t *configfile.Account) []string {
	names := make([]string, 0, len(t.Repositories))
	for _, r := range t.Repositories {
		names = append(names, r.Name)
	}
	return names
}
