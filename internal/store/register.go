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
// and onboarding know them before any event arrives. A repository already
// known is left as it is. It returns how many were new.
func (s *Store) RegisterRepositories(ctx context.Context, accountID string, repos []ReachedRepository) (int, error) {
	var added int
	err := s.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		for _, r := range repos {
			tag, err := tx.Exec(ctx, `INSERT INTO repositories (id, account_id, name, default_branch, managed_by, enabled)
				VALUES ($1, $2, $3, $4, 'forge', true) ON CONFLICT (account_id, name) DO NOTHING`,
				configfile.RepositoryID(accountID, r.FullName), accountID, r.FullName, r.DefaultBranch)
			if err != nil {
				return fmt.Errorf("store: register repository %s: %w", r.FullName, err)
			}
			added += int(tag.RowsAffected())
		}
		return nil
	})
	return added, err
}
