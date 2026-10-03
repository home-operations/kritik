package worker

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/metrics"
	"github.com/home-operations/kritika/internal/store"
)

// Rescue pace: how often the leader looks, and how many jobs and runs one
// pass takes on.
const (
	rescueInterval = 30 * time.Second
	rescueBatch    = 100
)

// rescueReason is what a rescued job's attempt, and the review and run it
// left, record as their error.
const rescueReason = "worker heartbeat lost"

// RunDeleter deletes a run's Kubernetes Job, pod and Secret with it; a Job
// already gone counts as deleted.
type RunDeleter interface {
	DeleteRun(ctx context.Context, runID string) error
}

// rescueStore is the subset of *store.Store the Rescuer needs, narrowed so
// it can be exercised in tests with a fake.
type rescueStore interface {
	AbandonedJobs(ctx context.Context, stale time.Duration, limit int) ([]store.AbandonedJob, error)
	RescueJob(ctx context.Context, job store.AbandonedJob, stale time.Duration, reason string) (bool, error)
	JobHead(ctx context.Context, jobID int64) (store.JobHead, bool, error)
	OrphanedRuns(ctx context.Context, limit int) ([]store.OrphanedRun, error)
	RevokeGatewayTokens(ctx context.Context, runID string) error
	EndOrphanedRun(ctx context.Context, run store.OrphanedRun, reason string) error
	SweepJobHeartbeats(ctx context.Context) (int64, error)
}

// Rescuer hands the jobs of a replica that died back to the queue, and
// reaps the runner Jobs they left. River's own rescuer must wait out the
// longest job timeout before it may touch a running job, hours; a job
// whose JobHeartbeat has gone stale is known dead long before that. A
// leader duty.
type Rescuer struct {
	Store rescueStore
	// Runs deletes the Kubernetes Job of an orphaned run; nil when runs
	// are not Jobs, as with the local executor.
	Runs RunDeleter
	// Current and Forges report a review that will not be tried again on
	// its head commit; without them nothing is reported.
	Current *configfile.Current
	Forges  forge.Clients
	Logger  *slog.Logger
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// every overrides rescueInterval, and stale jobAbandonedAfter.
	every, stale time.Duration
}

// Run rescues every rescueInterval until ctx ends. A pass that fails in
// part is logged and the rest tried again next time.
func (r *Rescuer) Run(ctx context.Context) {
	t := time.NewTicker(cmp.Or(r.every, rescueInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := r.Rescue(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Warn("abandoned jobs not all rescued", "error", err)
		}
	}
}

// Rescue makes one pass: jobs whose heartbeat went stale are handed back
// to River, then every unfinished run whose job is no longer running is
// reaped, and the heartbeats of ended jobs are swept. The two are
// independent, each idempotent, so a reap the API server refuses is tried
// again next pass without the job waiting on it.
func (r *Rescuer) Rescue(ctx context.Context) error {
	stale := cmp.Or(r.stale, jobAbandonedAfter)
	var errs []error
	abandoned, err := r.Store.AbandonedJobs(ctx, stale, rescueBatch)
	if err != nil {
		errs = append(errs, err)
	}
	for _, job := range abandoned {
		rescued, err := r.Store.RescueJob(ctx, job, stale, rescueReason)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !rescued {
			continue
		}
		state := string(job.RescueState())
		r.Logger.Warn("job rescued from a dead worker", "job", job.ID, "kind", job.Kind, "attempt", job.Attempt, "state", state)
		r.Metrics.JobRescued(job.Kind, state)
		if job.Kind == (jobs.ReviewArgs{}).Kind() && job.RescueState() != rivertype.JobStateRetryable {
			if err := r.reportEnded(ctx, job); err != nil {
				r.Logger.Warn("commit status not set for a rescued review", "job", job.ID, "error", err)
			}
		}
	}
	orphaned, err := r.Store.OrphanedRuns(ctx, rescueBatch)
	if err != nil {
		errs = append(errs, err)
	}
	for _, run := range orphaned {
		if err := r.reap(ctx, run); err != nil {
			errs = append(errs, err)
			continue
		}
		r.Logger.Warn("orphaned runner run reaped", "run", run.ID, "kind", run.Kind)
	}
	if _, err := r.Store.SweepJobHeartbeats(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reportEnded replaces the pending status on the head of a review job
// that was rescued into a state River does not run again: its dead worker
// set the status and nothing else would replace it.
func (r *Rescuer) reportEnded(ctx context.Context, job store.AbandonedJob) error {
	if r.Current == nil || r.Forges == nil {
		return nil
	}
	head, ok, err := r.Store.JobHead(ctx, job.ID)
	if err != nil || !ok {
		return err
	}
	file := r.Current.Get()
	account, ok := file.AccountByID(head.AccountID)
	if !ok {
		return nil
	}
	in := file.ConnectionFor(account)
	if in == nil {
		return nil
	}
	client, err := r.Forges.For(ctx, in, head.Repository)
	if err != nil {
		return err
	}
	desc := "kritika: review failed"
	if job.CancelRequested {
		desc = "kritika: review canceled"
	}
	owner, repo, _ := strings.Cut(head.Repository, "/")
	return client.SetStatus(ctx, owner, repo, head.HeadSHA, forge.StatusError, desc)
}

// reap deletes run's Job, revokes its gateway tokens and ends it. The Job
// goes first: a run is ended only once nothing of it can still spend.
func (r *Rescuer) reap(ctx context.Context, run store.OrphanedRun) error {
	if r.Runs != nil {
		if err := r.Runs.DeleteRun(ctx, run.ID); err != nil {
			return err
		}
	}
	if err := r.Store.RevokeGatewayTokens(ctx, run.ID); err != nil {
		return err
	}
	return r.Store.EndOrphanedRun(ctx, run, rescueReason)
}
