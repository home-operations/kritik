package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritika/internal/adapter"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/metrics"
	"github.com/home-operations/kritika/internal/store"
)

// Base is what every worker shares: the store, the live configuration,
// forge clients, logging and metrics, plus the lookups each job starts
// with.
type Base struct {
	Store   *store.Store
	Current *configfile.Current
	Forges  forge.Clients
	Logger  *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
}

// account finds the job's account in the current file. An account that has
// been removed cancels the job: it will not come back by retrying.
func (b *Base) account(file *configfile.File, id string) (*configfile.Account, error) {
	if t, ok := file.AccountByID(id); ok {
		return t, nil
	}
	return nil, river.JobCancel(fmt.Errorf("worker: account %s is not in the configuration", id))
}

// client resolves the connection serving account to its forge client for
// repo.
func (b *Base) client(ctx context.Context, file *configfile.File, account *configfile.Account, repo string) (forge.Client, error) {
	in := file.ConnectionFor(account)
	if in == nil {
		return nil, river.JobCancel(fmt.Errorf("worker: no connection serves account %s", account.Key()))
	}
	return b.Forges.For(ctx, in, repo)
}

// releaseTimeout bounds the lease release after the job's context is gone.
const releaseTimeout = 10 * time.Second

// detachTimeout bounds work that runs on past the job's ctx once the model
// has answered: publishing, and ending a review whose job ended. It must
// still finish then, but a hung forge or database call must not hold the
// job forever.
const detachTimeout = 2 * time.Minute

// detach is ctx without its cancellation, bounded by detachTimeout.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachTimeout)
}

// withLease runs fn while holding one of the account's slots on key,
// records the wait, and releases the slot afterwards even when the job's
// context has been cancelled.
func (b *Base) withLease(
	ctx context.Context, account *configfile.Account, key string, slots int, jobID int64, fn func(ctx context.Context) error,
) error {
	waited := time.Now()
	l, err := b.Store.AcquireLease(ctx, account.ID(), key, slots, jobID)
	if err != nil {
		return err
	}
	b.Metrics.LeaseWait(account.Key(), key, time.Since(waited))
	defer b.releaseLease(ctx, b.Logger, l, key)
	return fn(ctx)
}

// releaseLease releases l on a context of its own, since the job's has
// usually ended by the time a lease is let go, and logs a failure to
// logger, which carries whatever the caller knows of the job.
func (b *Base) releaseLease(ctx context.Context, logger *slog.Logger, l *store.Lease, key string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := l.Release(rctx); err != nil {
		logger.Warn("lease not released", "key", key, "error", err)
	}
}

// recorder writes model calls to the transcript view.
func (b *Base) recorder() adapter.Recorder {
	return adapter.Recorder{Store: b.Store, Metrics: b.Metrics}
}
