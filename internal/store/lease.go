package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// Lease timing. A holder renews every heartbeat; a lease older than expiry
// belongs to a process that died and is free to take.
const (
	leaseHeartbeat = 30 * time.Second
	leaseExpiry    = 2 * time.Minute
)

// How long AcquireLease waits between tries for a slot, per try.
const (
	leasePollMin = 2 * time.Second
	leasePollMax = 30 * time.Second
)

// Backoff is base doubled n times, capped at limit, then jittered into its
// upper half, so waiters that started together do not try again together.
func Backoff(n int, base, limit time.Duration) time.Duration {
	d := base
	for i := 0; i < n && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	return d/2 + rand.N(d/2+1)
}

// Lease is one held slot of model_leases: an account's concurrency on one
// model key, renewed until released.
type Lease struct {
	st        *Store
	accountID string
	modelKey  string
	slot      int
	jobID     int64
	cancel    context.CancelFunc
	done      chan struct{}
}

// AcquireLease blocks until a slot for (account, model) is free or ctx
// ends. slots is the account's concurrency limit; rows are created on
// demand so a raised limit takes effect on the next review and a lowered
// one leaves the extra rows unused.
func (s *Store) AcquireLease(ctx context.Context, accountID, modelKey string, slots int, jobID int64) (*Lease, error) {
	for try := 0; ; try++ {
		l, err := s.TakeLease(ctx, accountID, modelKey, slots, jobID)
		if err != nil || l != nil {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("store: waiting for a %s slot: %w", modelKey, ctx.Err())
		case <-time.After(Backoff(try, leasePollMin, leasePollMax)):
		}
	}
}

// TakeLease claims a free slot for (account, model) without waiting: nil
// when every slot is held.
func (s *Store) TakeLease(ctx context.Context, accountID, modelKey string, slots int, jobID int64) (*Lease, error) {
	slot := 0
	err := s.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO model_leases (account_id, model_key, slot)
			SELECT $1, $2, s FROM generate_series(1, $3::int) AS s ON CONFLICT DO NOTHING`, accountID, modelKey, slots); err != nil {
			return fmt.Errorf("store: ensure lease slots: %w", err)
		}
		err := tx.QueryRow(ctx, `UPDATE model_leases SET job_id = $3, expires_at = now() + $4::interval
			WHERE (account_id, model_key, slot) = (
				SELECT account_id, model_key, slot FROM model_leases
				WHERE account_id = $1 AND model_key = $2 AND slot <= $5 AND (job_id IS NULL OR expires_at < now())
				ORDER BY slot LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING slot`, accountID, modelKey, jobID, leaseExpiry, slots).Scan(&slot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: claim lease: %w", err)
		}
		return nil
	})
	if err != nil || slot == 0 {
		return nil, err
	}
	l := &Lease{st: s, accountID: accountID, modelKey: modelKey, slot: slot, jobID: jobID, done: make(chan struct{})}
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel = cancel
	go l.heartbeat(hctx)
	return l, nil
}

// SlotFree reports whether (account, model) has a slot no live lease
// holds, without claiming it: a hint that a review about to start its
// runner will find one when it needs it.
func (s *Store) SlotFree(ctx context.Context, accountID, modelKey string, slots int) (bool, error) {
	var held int
	err := s.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM model_leases WHERE account_id = $1 AND model_key = $2 AND slot <= $3
			AND job_id IS NOT NULL AND expires_at >= now()`, accountID, modelKey, slots).Scan(&held)
	})
	if err != nil {
		return false, fmt.Errorf("store: read lease slots: %w", err)
	}
	return held < slots, nil
}

func (l *Lease) heartbeat(ctx context.Context) {
	defer close(l.done)
	t := time.NewTicker(leaseHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A failed renewal is not fatal here: the slot expires and
			// another process may take it, which only means one extra
			// concurrent call for a while.
			_ = l.st.WithAccount(ctx, l.accountID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE model_leases SET expires_at = now() + $4::interval
					WHERE account_id = $1 AND model_key = $2 AND slot = $3 AND job_id = $5`,
					l.accountID, l.modelKey, l.slot, leaseExpiry, l.jobID)
				return err
			})
		}
	}
}

// Release frees the slot. ctx should outlive job cancellation so the slot
// is not left to expire.
func (l *Lease) Release(ctx context.Context) error {
	l.cancel()
	<-l.done
	return l.st.WithAccount(ctx, l.accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE model_leases SET job_id = NULL, expires_at = NULL
			WHERE account_id = $1 AND model_key = $2 AND slot = $3 AND job_id = $4`, l.accountID, l.modelKey, l.slot, l.jobID)
		if err != nil {
			return fmt.Errorf("store: release lease: %w", err)
		}
		return nil
	})
}

// CapReason says which of the account's caps its usage has reached, or "".
func CapReason(u MonthUsage, limits configfile.Limits) string {
	if limits.ReviewsPerDay > 0 && u.ReviewsToday >= int64(limits.ReviewsPerDay) {
		return fmt.Sprintf("reviewsPerDay (%d) reached", limits.ReviewsPerDay)
	}
	if limits.TokensPerMonth > 0 && u.Tokens >= limits.TokensPerMonth {
		return fmt.Sprintf("tokensPerMonth (%d) reached", limits.TokensPerMonth)
	}
	return ""
}

// AccountUsage is what the account's caps count: completed reviews today
// and tokens this month.
func (s *Store) AccountUsage(ctx context.Context, accountID string) (MonthUsage, error) {
	var m MonthUsage
	err := s.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		m, err = ReadMonthUsage(ctx, tx)
		return err
	})
	return m, err
}

// CapReached says which of the account's caps is exhausted, or "". With
// no cap set it reads nothing.
func (s *Store) CapReached(ctx context.Context, accountID string, limits configfile.Limits) (string, error) {
	if limits.ReviewsPerDay <= 0 && limits.TokensPerMonth <= 0 {
		return "", nil
	}
	u, err := s.AccountUsage(ctx, accountID)
	if err != nil {
		return "", err
	}
	return CapReason(u, limits), nil
}
