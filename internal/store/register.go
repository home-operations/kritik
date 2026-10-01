package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/configfile"
)

// ReachedRepository is a repository as the forge reports it. Traits is nil
// when the report names it without saying what it is, as an installation
// event does; the row keeps what it knew, or starts as neither archived
// nor a fork.
type ReachedRepository struct {
	FullName, DefaultBranch string
	Traits                  *configfile.RepoTraits
}

// RegisterRepositories records the repositories of account accountID that
// its connection's App reaches, as a webhook from each would, so polling
// and onboarding know them before any event arrives. It returns how many
// were new.
func (s *Store) RegisterRepositories(ctx context.Context, accountID string, repos []ReachedRepository) (int, error) {
	var added int
	err := s.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		for _, r := range repos {
			_, isNew, err := EnsureRepository(ctx, tx, accountID, r)
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

// EnsureRepository records repository r of account accountID as the forge
// names it, spelling included, and returns its id and whether it is new. A
// row the forge manages is enabled, since the forge just named it; one the
// spec lists keeps its enabled flag. A DefaultBranch of "" leaves the known
// one, and so does a nil Traits the known traits.
func EnsureRepository(ctx context.Context, tx pgx.Tx, accountID string, r ReachedRepository) (id string, isNew bool, err error) {
	var archived, fork *bool
	if r.Traits != nil {
		archived, fork = &r.Traits.Archived, &r.Traits.Fork
	}
	// xmax is 0 only on a row the statement inserted, not one it updated.
	err = tx.QueryRow(ctx, `
		INSERT INTO repositories (id, account_id, name, default_branch, managed_by, enabled, archived, fork)
		VALUES ($1, $2, $3, $4, 'forge', true, coalesce($5, false), coalesce($6, false))
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			default_branch = CASE WHEN EXCLUDED.default_branch <> '' THEN EXCLUDED.default_branch ELSE repositories.default_branch END,
			enabled = CASE WHEN repositories.managed_by = 'forge' THEN true ELSE repositories.enabled END,
			disabled_at = CASE WHEN repositories.managed_by = 'forge' THEN NULL ELSE repositories.disabled_at END,
			archived = coalesce($5, repositories.archived),
			fork = coalesce($6, repositories.fork),
			updated_at = now()
		RETURNING id, xmax = 0`,
		configfile.RepositoryID(accountID, r.FullName), accountID, r.FullName, r.DefaultBranch, archived, fork).Scan(&id, &isNew)
	if err != nil {
		return "", false, fmt.Errorf("store: ensure repository %s: %w", r.FullName, err)
	}
	return id, isNew, nil
}

// TurnedOn is the choice an admin made for the repository with id, nil
// when none was or the repository is not known yet.
func TurnedOn(ctx context.Context, tx pgx.Tx, id string) (*bool, error) {
	var on *bool
	err := tx.QueryRow(ctx, `SELECT turned_on FROM repositories WHERE id = $1`, id).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read whether repository %s is turned on: %w", id, err)
	}
	return on, nil
}

// TurnOn records an admin's choice to review the repository with id or
// not, and when it was made.
func TurnOn(ctx context.Context, tx pgx.Tx, id string, on bool) error {
	tag, err := tx.Exec(ctx, `UPDATE repositories SET turned_on = $2, turned_at = now(), updated_at = now() WHERE id = $1`, id, on)
	if err != nil {
		return fmt.Errorf("store: turn repository %s on or off: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
