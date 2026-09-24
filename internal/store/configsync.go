package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// ApplyConfig upserts the file's tenants, installations and listed
// repositories as file-managed rows, disables file-managed rows the file no
// longer declares, and records the applied hash. It runs as the owner in one
// transaction, so a replica reading config_state never sees a half-applied
// file. Only the leader calls it.
func (s *Store) ApplyConfig(ctx context.Context, f *configfile.File, leader string) error {
	if s.owner == nil {
		return errors.New("store: applying configuration needs the owner DSN")
	}
	tx, err := s.owner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin config apply: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	seenTenants := make([]string, 0, len(f.Tenants))
	seenInstallations := []string{}
	for i := range f.Tenants {
		t := &f.Tenants[i]
		seenTenants = append(seenTenants, t.Slug)
		tenantID, err := upsertTenant(ctx, tx, t)
		if err != nil {
			return err
		}
		installationIDs := map[string]string{}
		for j := range t.Installations {
			in := &t.Installations[j]
			seenInstallations = append(seenInstallations, in.Name)
			id, err := upsertInstallation(ctx, tx, tenantID, in)
			if err != nil {
				return err
			}
			installationIDs[in.Name] = id
		}
		for j := range t.Repositories {
			r := &t.Repositories[j]
			in := f.InstallationFor(t, r.Name)
			if err := upsertRepository(ctx, tx, tenantID, installationIDs[in.Name], r); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
			WHERE tenant_id = $1 AND managed_by = 'file' AND enabled AND name <> ALL($2)`,
			tenantID, repoNames(t)); err != nil {
			return fmt.Errorf("store: disable removed repositories: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE installations SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE managed_by = 'file' AND enabled AND name <> ALL($1)`, seenInstallations); err != nil {
		return fmt.Errorf("store: disable removed installations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tenants SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE managed_by = 'file' AND enabled AND slug <> ALL($1)`, seenTenants); err != nil {
		return fmt.Errorf("store: disable removed tenants: %w", err)
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
		INSERT INTO tenants (id, slug, managed_by, settings) VALUES ($1, $2, 'file', $3)
		ON CONFLICT (slug) DO UPDATE SET
			managed_by = 'file', settings = EXCLUDED.settings, enabled = true, disabled_at = NULL, updated_at = now()
		RETURNING id`, t.ID(), t.Slug, settings).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: upsert tenant %s: %w", t.Slug, err)
	}
	return id, nil
}

func upsertInstallation(ctx context.Context, tx pgx.Tx, tenantID string, in *configfile.Installation) (string, error) {
	kind := "token"
	if in.App != nil {
		kind = "app"
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO installations (id, tenant_id, name, forge, host, account, credential_kind, managed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'file')
		ON CONFLICT (name) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id, forge = EXCLUDED.forge, host = EXCLUDED.host, account = EXCLUDED.account,
			credential_kind = EXCLUDED.credential_kind, managed_by = 'file', enabled = true, disabled_at = NULL, updated_at = now()
		RETURNING id`, in.ID(), tenantID, in.Name, string(in.Forge), in.Host, in.Account, kind).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: upsert installation %s: %w", in.Name, err)
	}
	return id, nil
}

func upsertRepository(ctx context.Context, tx pgx.Tx, tenantID, installationID string, r *configfile.Repository) error {
	settings, err := json.Marshal(map[string]any{"filter": r.Filter, "konflate": r.Konflate, "ignore": r.Ignore})
	if err != nil {
		return fmt.Errorf("store: encode repository settings: %w", err)
	}
	enabled := r.Enabled == nil || *r.Enabled
	_, err = tx.Exec(ctx, `
		INSERT INTO repositories (id, tenant_id, installation_id, name, settings, managed_by, enabled, disabled_at)
		VALUES ($1, $2, $3, $4, $5, 'file', $6, CASE WHEN $6 THEN NULL ELSE now() END)
		ON CONFLICT (installation_id, name) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id, settings = EXCLUDED.settings, managed_by = 'file', enabled = EXCLUDED.enabled,
			disabled_at = CASE WHEN EXCLUDED.enabled THEN NULL ELSE coalesce(repositories.disabled_at, now()) END,
			updated_at = now()`,
		configfile.RepositoryID(installationID, r.Name), tenantID, installationID, r.Name, settings, enabled)
	if err != nil {
		return fmt.Errorf("store: upsert repository %s: %w", r.Name, err)
	}
	return nil
}

func repoNames(t *configfile.Tenant) []string {
	names := make([]string, 0, len(t.Repositories))
	for _, r := range t.Repositories {
		names = append(names, r.Name)
	}
	return names
}
