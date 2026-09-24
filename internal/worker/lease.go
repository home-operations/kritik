package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
)

// Lease timing. A holder renews every heartbeat; a lease older than expiry
// belongs to a worker that died and is free to take.
const (
	leaseHeartbeat = 30 * time.Second
	leaseExpiry    = 2 * time.Minute
	leasePoll      = 5 * time.Second
)

// lease is one held slot of model_leases.
type lease struct {
	st       *store.Store
	tenantID string
	modelKey string
	slot     int
	jobID    int64
	cancel   context.CancelFunc
	done     chan struct{}
}

// acquireLease blocks until a slot for (tenant, model) is free or ctx ends.
// slots is the tenant's concurrency limit; rows are created on demand so a
// raised limit takes effect on the next review and a lowered one leaves
// the extra rows unused.
func acquireLease(ctx context.Context, st *store.Store, tenantID, modelKey string, slots int, jobID int64) (*lease, error) {
	for {
		slot, err := tryLease(ctx, st, tenantID, modelKey, slots, jobID)
		if err != nil {
			return nil, err
		}
		if slot > 0 {
			l := &lease{st: st, tenantID: tenantID, modelKey: modelKey, slot: slot, jobID: jobID, done: make(chan struct{})}
			hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			l.cancel = cancel
			go l.heartbeat(hctx)
			return l, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("worker: waiting for a %s slot: %w", modelKey, ctx.Err())
		case <-time.After(leasePoll):
		}
	}
}

func tryLease(ctx context.Context, st *store.Store, tenantID, modelKey string, slots int, jobID int64) (int, error) {
	slot := 0
	err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO model_leases (tenant_id, model_key, slot)
			SELECT $1, $2, s FROM generate_series(1, $3::int) AS s ON CONFLICT DO NOTHING`, tenantID, modelKey, slots); err != nil {
			return fmt.Errorf("worker: ensure lease slots: %w", err)
		}
		err := tx.QueryRow(ctx, `UPDATE model_leases SET job_id = $3, expires_at = now() + $4::interval
			WHERE (tenant_id, model_key, slot) = (
				SELECT tenant_id, model_key, slot FROM model_leases
				WHERE tenant_id = $1 AND model_key = $2 AND slot <= $5 AND (job_id IS NULL OR expires_at < now())
				ORDER BY slot LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING slot`, tenantID, modelKey, jobID, leaseExpiry, slots).Scan(&slot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("worker: claim lease: %w", err)
		}
		return nil
	})
	return slot, err
}

func (l *lease) heartbeat(ctx context.Context) {
	defer close(l.done)
	t := time.NewTicker(leaseHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A failed renewal is not fatal here: the slot expires and
			// another worker may take it, which only means one extra
			// concurrent call for a while.
			_ = l.st.WithTenant(ctx, l.tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE model_leases SET expires_at = now() + $4::interval
					WHERE tenant_id = $1 AND model_key = $2 AND slot = $3 AND job_id = $5`,
					l.tenantID, l.modelKey, l.slot, leaseExpiry, l.jobID)
				return err
			})
		}
	}
}

// release frees the slot. ctx should outlive job cancellation so the slot
// is not left to expire.
func (l *lease) release(ctx context.Context) error {
	l.cancel()
	<-l.done
	return l.st.WithTenant(ctx, l.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE model_leases SET job_id = NULL, expires_at = NULL
			WHERE tenant_id = $1 AND model_key = $2 AND slot = $3 AND job_id = $4`, l.tenantID, l.modelKey, l.slot, l.jobID)
		if err != nil {
			return fmt.Errorf("worker: release lease: %w", err)
		}
		return nil
	})
}
