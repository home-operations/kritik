package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunSecretsToSweep returns the tenant's runner_runs whose job-scoped
// Secret may still exist and that no run can need any more: created more
// than settle ago, not yet swept, and either finished or left unfinished
// for longer than abandoned. A worker records finished_at when its run
// ends, so an unfinished row is either still running or was left by a
// worker that died; abandoned must outlast any Job such a worker could
// have started, so a live run's Secret is never deleted under it.
func (s *Store) RunSecretsToSweep(ctx context.Context, tenantID string, settle, abandoned time.Duration, limit int) ([]string, error) {
	var ids []string
	err := s.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM runner_runs
			WHERE secret_swept_at IS NULL
			  AND created_at < now() - make_interval(secs => $1)
			  AND (finished_at IS NOT NULL OR created_at < now() - make_interval(secs => $2))
			ORDER BY created_at LIMIT $3`,
			settle.Seconds(), abandoned.Seconds(), limit)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list run secrets to sweep: %w", err)
	}
	return ids, nil
}

// MarkRunSecretsSwept stamps secret_swept_at on the tenant's runs so the
// sweep does not visit them again. Rows already stamped keep their stamp.
func (s *Store) MarkRunSecretsSwept(ctx context.Context, tenantID string, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	err := s.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET secret_swept_at = now() WHERE id = ANY($1::uuid[]) AND secret_swept_at IS NULL`, runIDs)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: mark run secrets swept: %w", err)
	}
	return nil
}
