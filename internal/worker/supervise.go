package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/store"
)

// Causes supervision cancels a run with.
var (
	errSuperseded    = errors.New("worker: a newer head superseded the run")
	errHeartbeatLost = errors.New("worker: runner heartbeat lost")
)

const (
	// superviseInterval is how often a running run is checked.
	superviseInterval = 10 * time.Second
	// heartbeatStale is how old a runner's heartbeat may get before the run
	// is treated as dead: six missed beats, to ride out a slow database.
	heartbeatStale = 90 * time.Second
)

// supervision is what the worker watches while a runner works.
type supervision struct {
	every, stale time.Duration
	// heartbeat reports the age of the run's last heartbeat, and whether
	// the runner has stamped one at all.
	heartbeat func(context.Context) (age time.Duration, started bool, err error)
	// head reads the pull request's current head; nil for runs no newer
	// head can supersede.
	head func(context.Context) (string, error)
	want string
	// logger reports a check that could not be made.
	logger *slog.Logger
}

// supervise runs spec, cancelling it when its heartbeat goes stale or a
// newer head supersedes it. It returns the executor's result and the cause
// it cancelled with, nil when it did not.
func supervise(ctx context.Context, s supervision, exec executor.Executor, spec executor.Spec) (executor.Result, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan executor.Result, 1)
	go func() { done <- exec.Run(rctx, spec) }()
	t := time.NewTicker(s.every)
	defer t.Stop()
	var cause error
	for {
		select {
		case res := <-done:
			return res, cause
		case <-t.C:
		}
		if cause != nil || rctx.Err() != nil {
			continue
		}
		if cause = s.check(rctx); cause != nil {
			cancel(cause)
		}
	}
}

// check returns a reason to end the run, or nil. A read that fails is
// logged and retried on the next tick rather than ending the run.
func (s supervision) check(ctx context.Context) error {
	if s.head != nil {
		head, err := s.head(ctx)
		switch {
		case err != nil:
			s.logger.Warn("supervise: read head", "error", err)
		case head != s.want:
			return errSuperseded
		}
	}
	age, started, err := s.heartbeat(ctx)
	switch {
	case err != nil:
		s.logger.Warn("supervise: read heartbeat", "error", err)
	case started && age > s.stale:
		return errHeartbeatLost
	}
	return nil
}

// runSupervision builds the supervision of one runner run. Heartbeat age is
// measured by the database's clock, the same clock that stamped it. prID
// empty skips the supersede check.
func runSupervision(st *store.Store, tenantID, runID, prID, head string, every time.Duration, logger *slog.Logger) supervision {
	if every <= 0 {
		every = superviseInterval
	}
	s := supervision{
		every: every, stale: heartbeatStale, logger: logger,
		heartbeat: func(ctx context.Context) (time.Duration, bool, error) {
			var seconds *float64
			err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT extract(epoch FROM now() - heartbeat_at)::float8 FROM runner_runs WHERE id = $1`, runID).Scan(&seconds)
			})
			if err != nil {
				return 0, false, fmt.Errorf("worker: read heartbeat: %w", err)
			}
			if seconds == nil {
				return 0, false, nil
			}
			return time.Duration(*seconds * float64(time.Second)), true, nil
		},
	}
	if prID != "" {
		s.want = head
		s.head = func(ctx context.Context) (string, error) {
			var current string
			err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE id = $1`, prID).Scan(&current)
			})
			if err != nil {
				return "", fmt.Errorf("worker: read head: %w", err)
			}
			return current, nil
		}
	}
	return s
}
