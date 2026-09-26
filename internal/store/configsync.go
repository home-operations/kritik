package store

import (
	"context"
	"encoding/json"
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
// file rejects a slug or name both declare, so this guards the database
// rather than being expected.
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

// ApplyConfig upserts the file's tenants, installations and listed
// repositories as rows managed by each tenant's origin, disables file and
// dashboard rows the file no longer declares, and records the applied hash.
// A row declared again by another origin (a file tenant removed and a
// dashboard tenant of the same slug added, or the reverse) is disabled and
// then taken over; an enabled row is never taken over, and neither is an
// installation another tenant holds (both ErrManagedBy). It runs as the
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
	for i := range f.Tenants {
		t := &f.Tenants[i]
		tenantID, err := upsertTenant(ctx, tx, t)
		if err != nil {
			return err
		}
		installationIDs := map[string]string{}
		for j := range t.Installations {
			in := &t.Installations[j]
			id, err := upsertInstallation(ctx, tx, tenantID, t.Origin(), in)
			if err != nil {
				return err
			}
			installationIDs[in.Name] = id
		}
		for j := range t.Repositories {
			r := &t.Repositories[j]
			in := f.InstallationFor(t, r)
			if err := upsertRepository(ctx, tx, tenantID, installationIDs[in.Name], t.Origin(), r); err != nil {
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

// disableUndeclared disables every file or dashboard tenant and installation
// f does not declare with that same origin, then every file or dashboard
// repository under a disabled tenant or installation or no longer listed by
// its tenant. The upserts that follow re-enable what f still declares.
// Forge-discovered repositories are left alone.
func disableUndeclared(ctx context.Context, tx pgx.Tx, f *configfile.File) error {
	slugs, slugOrigins := make([]string, 0, len(f.Tenants)), make([]string, 0, len(f.Tenants))
	var names, nameOrigins []string
	for i := range f.Tenants {
		t := &f.Tenants[i]
		slugs, slugOrigins = append(slugs, t.Slug), append(slugOrigins, string(t.Origin()))
		for j := range t.Installations {
			names, nameOrigins = append(names, t.Installations[j].Name), append(nameOrigins, string(t.Origin()))
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tenants SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE managed_by IN ('file', 'dashboard') AND enabled
			AND (slug, managed_by) NOT IN (SELECT * FROM unnest($1::text[], $2::text[]))`, slugs, slugOrigins); err != nil {
		return fmt.Errorf("store: disable removed tenants: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE installations SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE managed_by IN ('file', 'dashboard') AND enabled
			AND (name, managed_by) NOT IN (SELECT * FROM unnest($1::text[], $2::text[]))`, names, nameOrigins); err != nil {
		return fmt.Errorf("store: disable removed installations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE repositories r SET enabled = false, disabled_at = coalesce(r.disabled_at, now()), updated_at = now()
		WHERE r.managed_by IN ('file', 'dashboard') AND r.enabled AND (
			EXISTS (SELECT 1 FROM tenants t WHERE t.id = r.tenant_id AND NOT t.enabled)
			OR EXISTS (SELECT 1 FROM installations i WHERE i.id = r.installation_id AND NOT i.enabled))`); err != nil {
		return fmt.Errorf("store: disable repositories of removed tenants and installations: %w", err)
	}
	for i := range f.Tenants {
		t := &f.Tenants[i]
		if _, err := tx.Exec(ctx, `
			UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
			WHERE tenant_id = $1 AND managed_by IN ('file', 'dashboard') AND enabled AND name <> ALL($2)`,
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

func upsertTenant(ctx context.Context, tx pgx.Tx, t *configfile.Tenant) (string, error) {
	settings, err := json.Marshal(map[string]any{
		"models": t.Models, "filter": t.Filter, "forks": t.Forks, "limits": t.Limits, "runner": t.Runner,
	})
	if err != nil {
		return "", fmt.Errorf("store: encode tenant settings: %w", err)
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO tenants (id, slug, managed_by, settings) VALUES ($1, $2, $3, $4)
		ON CONFLICT (slug) DO UPDATE SET
			managed_by = EXCLUDED.managed_by, settings = EXCLUDED.settings, enabled = true, disabled_at = NULL, updated_at = now()
		WHERE tenants.managed_by = EXCLUDED.managed_by OR NOT tenants.enabled
		RETURNING id`, t.ID(), t.Slug, string(t.Origin()), settings).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", managedByConflict(ctx, tx, "tenant "+t.Slug, t.Origin(), `SELECT managed_by FROM tenants WHERE slug = $1`, t.Slug)
	}
	if err != nil {
		return "", fmt.Errorf("store: upsert tenant %s: %w", t.Slug, err)
	}
	return id, nil
}

func upsertInstallation(
	ctx context.Context, tx pgx.Tx, tenantID string, origin configfile.Origin, in *configfile.Installation,
) (string, error) {
	kind := "token"
	if in.App != nil {
		kind = "app"
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO installations (id, tenant_id, name, forge, host, account, credential_kind, managed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id, forge = EXCLUDED.forge, host = EXCLUDED.host, account = EXCLUDED.account,
			credential_kind = EXCLUDED.credential_kind, managed_by = EXCLUDED.managed_by, enabled = true, disabled_at = NULL,
			updated_at = now()
		WHERE installations.tenant_id = EXCLUDED.tenant_id AND (installations.managed_by = EXCLUDED.managed_by OR NOT installations.enabled)
		RETURNING id`, in.ID(), tenantID, in.Name, string(in.Forge), in.Host, in.Account, kind, string(origin)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another tenant's row keeps its history (pull requests, reviews,
		// repositories) under its own tenant id; handing the name over would
		// mix the two, so it stays with its tenant, enabled or not.
		return "", managedByConflict(ctx, tx, "installation "+in.Name, origin,
			`SELECT CASE WHEN tenant_id = $2 THEN managed_by ELSE 'another tenant' END FROM installations WHERE name = $1`,
			in.Name, tenantID)
	}
	if err != nil {
		return "", fmt.Errorf("store: upsert installation %s: %w", in.Name, err)
	}
	return id, nil
}

// upsertRepository also takes over a forge-discovered row: listing a
// repository the poller or a webhook already found makes it declared.
func upsertRepository(
	ctx context.Context, tx pgx.Tx, tenantID, installationID string, origin configfile.Origin, r *configfile.Repository,
) error {
	settings, err := json.Marshal(map[string]any{"filter": r.Filter, "konflate": r.Konflate, "ignore": r.Ignore})
	if err != nil {
		return fmt.Errorf("store: encode repository settings: %w", err)
	}
	enabled := r.Enabled == nil || *r.Enabled
	tag, err := tx.Exec(ctx, `
		INSERT INTO repositories (id, tenant_id, installation_id, name, settings, managed_by, enabled, disabled_at)
		VALUES ($1, $2, $3, $4, $5, $7, $6, CASE WHEN $6 THEN NULL ELSE now() END)
		ON CONFLICT (installation_id, name) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id, settings = EXCLUDED.settings, managed_by = EXCLUDED.managed_by, enabled = EXCLUDED.enabled,
			disabled_at = CASE WHEN EXCLUDED.enabled THEN NULL ELSE coalesce(repositories.disabled_at, now()) END,
			updated_at = now()
		WHERE repositories.managed_by IN (EXCLUDED.managed_by, 'forge') OR NOT repositories.enabled`,
		configfile.RepositoryID(installationID, r.Name), tenantID, installationID, r.Name, settings, enabled, string(origin))
	if err == nil && tag.RowsAffected() == 0 {
		return managedByConflict(ctx, tx, "repository "+r.Name, origin,
			`SELECT managed_by FROM repositories WHERE installation_id = $1 AND name = $2`, installationID, r.Name)
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

func repoNames(t *configfile.Tenant) []string {
	names := make([]string, 0, len(t.Repositories))
	for _, r := range t.Repositories {
		names = append(names, r.Name)
	}
	return names
}
