package worker

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// capReached returns a description of the account's cap that is exhausted,
// or "".
func capReached(ctx context.Context, st *store.Store, accountID string, limits configfile.Limits) (string, error) {
	if limits.ReviewsPerDay <= 0 && limits.TokensPerMonth <= 0 {
		return "", nil
	}
	u, err := readUsage(ctx, st, accountID)
	if err != nil {
		return "", err
	}
	return reached(u, limits), nil
}

// readUsage is what an account's caps count: completed reviews today and
// tokens this month.
func readUsage(ctx context.Context, st *store.Store, accountID string) (store.MonthUsage, error) {
	var m store.MonthUsage
	err := st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		m, err = store.ReadMonthUsage(ctx, tx)
		return err
	})
	if err != nil {
		return store.MonthUsage{}, fmt.Errorf("worker: read caps: %w", err)
	}
	return m, nil
}

// reached says which cap u has reached, or "".
func reached(u store.MonthUsage, limits configfile.Limits) string {
	if limits.ReviewsPerDay > 0 && u.ReviewsToday >= int64(limits.ReviewsPerDay) {
		return fmt.Sprintf("reviewsPerDay (%d) reached", limits.ReviewsPerDay)
	}
	if limits.TokensPerMonth > 0 && u.Tokens >= limits.TokensPerMonth {
		return fmt.Sprintf("tokensPerMonth (%d) reached", limits.TokensPerMonth)
	}
	return ""
}
