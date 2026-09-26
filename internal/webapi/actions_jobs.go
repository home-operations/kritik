package webapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/jobs"
)

// JobActions queues dashboard actions on River.
type JobActions struct {
	Queue *river.Client[pgx.Tx]
}

// Rerun implements Actions.
func (a JobActions) Rerun(ctx context.Context, tx pgx.Tx, tenantID, repositoryID string, number int) (int64, error) {
	id, err := jobs.EnqueueRerun(ctx, tx, a.Queue, tenantID, repositoryID, number)
	switch {
	case errors.Is(err, jobs.ErrNoHead):
		return 0, ErrNoHead
	case errors.Is(err, jobs.ErrRerunQueued):
		return 0, ErrRerunQueued
	}
	return id, err
}

// Cancel implements Actions.
func (a JobActions) Cancel(ctx context.Context, tx pgx.Tx, reviewID, by string) error {
	err := jobs.RequestCancel(ctx, tx, a.Queue, reviewID, by)
	if errors.Is(err, jobs.ErrNotCancelable) {
		return ErrNotCancelable
	}
	return err
}

// Reindex implements Actions.
func (a JobActions) Reindex(ctx context.Context, tx pgx.Tx, tenantID, repositoryID string) (int64, error) {
	id, err := jobs.EnqueueReindex(ctx, tx, a.Queue, tenantID, repositoryID)
	if errors.Is(err, jobs.ErrRepositoryNotFound) {
		return 0, ErrRepositoryNotFound
	}
	if errors.Is(err, jobs.ErrReindexQueued) {
		return 0, ErrReindexQueued
	}
	return id, err
}
