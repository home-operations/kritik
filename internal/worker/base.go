package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/store"
)

// Base is what every worker shares: the store, the live configuration,
// forge clients, logging and metrics, plus the lookups each job starts
// with.
type Base struct {
	Store   *store.Store
	Current *configfile.Current
	Forges  Forges
	Logger  *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
}

// tenant finds the job's tenant in the current file. A tenant that has
// been removed cancels the job: it will not come back by retrying.
func (b *Base) tenant(file *configfile.File, id string) (*configfile.Tenant, error) {
	for i := range file.Tenants {
		if file.Tenants[i].ID() == id {
			return &file.Tenants[i], nil
		}
	}
	return nil, river.JobCancel(fmt.Errorf("worker: tenant %s is not in the configuration", id))
}

// client resolves an installation by name to its forge client.
func (b *Base) client(
	ctx context.Context, file *configfile.File, installation string, externalID int64, repo string,
) (forge.Client, error) {
	in, _, ok := file.Installation(installation)
	if !ok {
		return nil, river.JobCancel(fmt.Errorf("worker: installation %s is not in the configuration", installation))
	}
	return b.Forges.For(ctx, in, externalID, repo)
}

// releaseTimeout bounds the lease release after the job's context is gone.
const releaseTimeout = 10 * time.Second

// withLease runs fn while holding one of the tenant's slots on key,
// records the wait, and releases the slot afterwards even when the job's
// context has been cancelled.
func (b *Base) withLease(
	ctx context.Context, tenant *configfile.Tenant, key string, slots int, jobID int64, fn func(ctx context.Context) error,
) error {
	waited := time.Now()
	l, err := acquireLease(ctx, b.Store, tenant.ID(), key, slots, jobID)
	if err != nil {
		return err
	}
	b.Metrics.LeaseWait(tenant.Slug, key, time.Since(waited))
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := l.release(rctx); err != nil {
			b.Logger.Warn("lease not released", "key", key, "error", err)
		}
	}()
	return fn(ctx)
}
