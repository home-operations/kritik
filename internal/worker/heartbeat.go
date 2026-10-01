package worker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// A replica's liveness on the River jobs it works: a beat every
// jobHeartbeatInterval, and a job whose last beat is older than
// jobAbandonedAfter belongs to a replica that died. Six missed beats, as a
// runner's heartbeat gets, so a slow database alone never has a live job
// rescued and run twice.
const (
	jobHeartbeatInterval = 30 * time.Second
	jobAbandonedAfter    = 6 * jobHeartbeatInterval
)

// jobHeartbeater stamps a job's heartbeat.
type jobHeartbeater interface {
	HeartbeatJob(ctx context.Context, jobID int64) error
}

// JobHeartbeat is River middleware that stamps a heartbeat on every job
// this replica works, once before the job starts and then every
// jobHeartbeatInterval until Work returns, so the leader's Rescuer can tell
// a job whose replica died from one still being worked (ADR-0024 §2.3
// covers a replica that stops; this covers one that does not get to).
type JobHeartbeat struct {
	river.MiddlewareDefaults
	Store  jobHeartbeater
	Logger *slog.Logger
	// every overrides jobHeartbeatInterval in tests.
	every time.Duration
}

// Work implements rivertype.WorkerMiddleware. A first beat that cannot be
// written fails the attempt: a job nobody could tell was alive must not
// start. Later beats go on past ctx, which a timeout or a cancel ends
// while the job is still this replica's to finish on detached contexts.
func (h *JobHeartbeat) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	if err := h.Store.HeartbeatJob(ctx, job.ID); err != nil {
		return fmt.Errorf("worker: first heartbeat of job %d: %w", job.ID, err)
	}
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go h.beat(hctx, job.ID, done)
	defer func() {
		cancel()
		<-done
	}()
	return doInner(ctx)
}

func (h *JobHeartbeat) beat(ctx context.Context, jobID int64, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(cmp.Or(h.every, jobHeartbeatInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// A missed beat is not fatal here: the job goes on, and only a run
		// of them has the leader rescue it.
		if err := h.Store.HeartbeatJob(ctx, jobID); err != nil && ctx.Err() == nil {
			h.Logger.Warn("job heartbeat not written", "job", jobID, "error", err)
		}
	}
}
