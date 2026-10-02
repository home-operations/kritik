package store

import (
	"context"
	"errors"
	"time"
)

// leaderKey is the advisory lock every leader-eligible replica competes for.
const leaderKey = "kritika-leader"

// RunAsLeader competes for the leader lock and, once held, calls lead with a
// context that is cancelled if the lock is lost or ctx ends. It returns when
// ctx ends, or when lead returns an error. Only one replica per database
// holds the lock at a time; the holder is the only replica that migrates
// and writes configuration.
//
// The lock is session-level on a dedicated connection held for the whole
// tenure, so it releases on its own if the process dies. The holder pings
// that connection every interval, because a dropped connection releases the
// lock server-side without the client noticing. A database that cannot be
// reached when the lock is tried, as during a failover, is tried again
// after retry rather than ending the process: a standby that restarts on
// every failover is no standby.
func (s *Store) RunAsLeader(ctx context.Context, retry time.Duration, lead func(ctx context.Context) error) error {
	if s.owner == nil {
		return errors.New("store: leadership needs the owner DSN")
	}
	for {
		held, err := s.tryLead(ctx, retry, lead)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if !held {
			s.logger.Debug("leader lock held elsewhere, retrying", "after", retry)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// tryLead makes one attempt. It reports whether the lock was held during
// this attempt; a false return means someone else has it, or the database
// did not answer, which is logged. The error is lead's own. The lock
// connection is only ever touched from this goroutine: lead runs on its own
// goroutine and never sees the connection.
func (s *Store) tryLead(ctx context.Context, interval time.Duration, lead func(ctx context.Context) error) (bool, error) {
	conn, err := s.owner.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("leader connection not acquired, retrying", "error", err, "after", interval)
		}
		return false, nil
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, leaderKey).Scan(&got); err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("leader lock not tried, retrying", "error", err, "after", interval)
		}
		return false, nil
	}
	if !got {
		return false, nil
	}
	s.logger.Info("leader lock acquired")

	leadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lead(leadCtx) }()

	t := time.NewTicker(interval)
	defer t.Stop()
	var leadErr error
loop:
	for {
		select {
		case leadErr = <-done:
			break loop
		case <-ctx.Done():
			cancel()
			leadErr = <-done
			break loop
		case <-t.C:
			if err := conn.Ping(ctx); err != nil {
				s.logger.Warn("leader lock connection lost, stepping down", "error", err)
				cancel()
				leadErr = <-done
				break loop
			}
		}
	}

	// Best effort: the session ends with the connection anyway.
	unlockCtx, unlockCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer unlockCancel()
	_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtext($1))`, leaderKey)
	s.logger.Info("leader lock released")
	return true, leadErr
}
