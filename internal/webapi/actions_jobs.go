package webapi

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/jobs"
)

// JobActions queues dashboard actions on River.
type JobActions struct {
	Queue *river.Client[pgx.Tx]
}

// Rerun implements Actions.
func (a JobActions) Rerun(ctx context.Context, tx pgx.Tx, accountID, repositoryID string, number int) (int64, error) {
	return jobs.EnqueueRerun(ctx, tx, a.Queue, accountID, repositoryID, number)
}

// Cancel implements Actions.
func (a JobActions) Cancel(ctx context.Context, tx pgx.Tx, reviewID string) error {
	return jobs.RequestCancel(ctx, tx, a.Queue, reviewID)
}

// Reindex implements Actions.
func (a JobActions) Reindex(ctx context.Context, tx pgx.Tx, accountID, repositoryID string) (int64, error) {
	return jobs.EnqueueReindex(ctx, tx, a.Queue, accountID, repositoryID)
}
