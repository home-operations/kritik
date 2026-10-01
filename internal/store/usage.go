package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Usage roles, matching the usage table's CHECK.
const (
	RoleReview    = "review"
	RoleEmbedding = "embedding"
	RoleFollowUp  = "followup"
)

// Usage is one model call charged to an account, and to the review it
// served when there is one: its whole prompt, cached part included, as
// input.
type Usage struct {
	AccountID, RepositoryID, ReviewID, Role, Model, Upstream string
	Input, Output                                            int64
	CostUSD                                                  float64
}

// InsertUsage records u, where the caps count it.
func InsertUsage(ctx context.Context, tx pgx.Tx, u Usage) error {
	if _, err := tx.Exec(ctx, `INSERT INTO usage
		(account_id, repository_id, review_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, nullif($3, '')::uuid, $4, $5, $6, $7, $8, $9)`,
		u.AccountID, u.RepositoryID, u.ReviewID, u.Role, u.Model, u.Upstream, u.Input, u.Output, u.CostUSD); err != nil {
		return fmt.Errorf("store: insert usage: %w", err)
	}
	return nil
}
