package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// ReachedRepository is a repository a connection's App reaches.
type ReachedRepository struct {
	FullName, DefaultBranch string
}

// RegisterRepositories records the repositories of account accountID that
// its connection's App reaches, as a webhook from each would, so polling
// and onboarding know them before any event arrives. It returns how many
// were new.
func (s *Store) RegisterRepositories(ctx context.Context, accountID string, repos []ReachedRepository) (int, error) {
	var added int
	err := s.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		for _, r := range repos {
			_, isNew, err := EnsureRepository(ctx, tx, accountID, r.FullName, r.DefaultBranch)
			if err != nil {
				return err
			}
			if isNew {
				added++
			}
		}
		return nil
	})
	return added, err
}

// EnsureRepository records repository fullName of account accountID as
// the forge names it, and returns its id and whether it is new. A row the
// forge manages is enabled, since the forge just named it; one the spec
// lists keeps its enabled flag. A defaultBranch of "" leaves the known one.
func EnsureRepository(ctx context.Context, tx pgx.Tx, accountID, fullName, defaultBranch string) (id string, isNew bool, err error) {
	// xmax is 0 only on a row the statement inserted, not one it updated.
	err = tx.QueryRow(ctx, `
		INSERT INTO repositories (id, account_id, name, default_branch, managed_by, enabled)
		VALUES ($1, $2, $3, $4, 'forge', true)
		ON CONFLICT (account_id, name) DO UPDATE SET
			default_branch = CASE WHEN EXCLUDED.default_branch <> '' THEN EXCLUDED.default_branch ELSE repositories.default_branch END,
			enabled = CASE WHEN repositories.managed_by = 'forge' THEN true ELSE repositories.enabled END,
			disabled_at = CASE WHEN repositories.managed_by = 'forge' THEN NULL ELSE repositories.disabled_at END,
			updated_at = now()
		RETURNING id, xmax = 0`,
		configfile.RepositoryID(accountID, fullName), accountID, fullName, defaultBranch).Scan(&id, &isNew)
	if err != nil {
		return "", false, fmt.Errorf("store: ensure repository %s: %w", fullName, err)
	}
	return id, isNew, nil
}
