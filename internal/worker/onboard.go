package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
)

// Onboarding pace: how often the feeder tops the window up, and how long a
// repository whose onboarding job ended without building an index waits to
// be offered again. River keeps finished jobs longer than that.
const (
	onboardInterval   = 30 * time.Second
	onboardRetryAfter = time.Hour
)

// Onboarder feeds onboarding index jobs to the index queue a few at a time:
// after a first install, or a model change that drops every index, all
// repositories need one at once, and inserting them all would put the
// first-declared account's hundreds ahead of everyone else's. It keeps at
// most indexing.onboardWindow of them queued or running, accounts taking turns and the
// repositories whose pull requests moved last going first. A leader duty.
type Onboarder struct {
	Store *store.Store
	Queue *river.Client[pgx.Tx]
	// Current's indexing.onboardWindow is how many onboarding jobs may be
	// queued or running at once.
	Current *configfile.Current
	Logger  *slog.Logger
}

// Run tops the window up every onboardInterval until ctx ends. A failed
// top-up is logged and tried again on the next tick.
func (o *Onboarder) Run(ctx context.Context) {
	t := time.NewTicker(onboardInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := o.Offer(ctx); err != nil && ctx.Err() == nil {
				o.Logger.Warn("onboarding not offered", "error", err)
			}
		}
	}
}

// Offer queues onboarding jobs up to the window, and none while the
// running configuration has no embedder.
func (o *Onboarder) Offer(ctx context.Context) error {
	f := o.Current.Get()
	if f.Embedding == nil {
		return nil
	}
	inFlight, err := o.Store.OnboardingInFlight(ctx)
	if err != nil {
		return err
	}
	window := f.OnboardWindow()
	room := window - inFlight
	if room <= 0 {
		return nil
	}
	refs, err := o.Store.OnboardCandidates(ctx, room, onboardRetryAfter, func(accountID, fullName string) bool {
		a, ok := f.AccountByID(accountID)
		return ok && f.Settings(a, fullName).Enabled
	})
	if err != nil || len(refs) == 0 {
		return err
	}
	params := make([]river.InsertManyParams, 0, len(refs))
	for _, r := range refs {
		args := jobs.IndexArgs{AccountID: r.AccountID, RepositoryID: r.ID, Trigger: jobs.TriggerOnboard}
		params = append(params, river.InsertManyParams{Args: args})
	}
	results, err := o.Queue.InsertMany(ctx, params)
	if err != nil {
		return fmt.Errorf("worker: enqueue onboarding: %w", err)
	}
	queued := 0
	for _, r := range results {
		if !r.UniqueSkippedAsDuplicate {
			queued++
		}
	}
	o.Logger.Info("onboarding index jobs queued", "queued", queued, "in_flight", inFlight, "window", window)
	return nil
}
