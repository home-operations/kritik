package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
)

// fakeHeartbeater records every beat and fails the first n.
type fakeHeartbeater struct {
	mu    sync.Mutex
	beats []int64
	fail  int
}

func (f *fakeHeartbeater) HeartbeatJob(_ context.Context, jobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("db down")
	}
	f.beats = append(f.beats, jobID)
	return nil
}

func (f *fakeHeartbeater) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.beats)
}

func TestJobHeartbeatBeatsWhileTheJobRuns(t *testing.T) {
	store := &fakeHeartbeater{}
	h := &JobHeartbeat{Store: store, Logger: slog.New(slog.DiscardHandler), every: 5 * time.Millisecond}
	job := &rivertype.JobRow{ID: 42}
	var atStart int
	// The job's own context ends before Work does, as a timeout or a
	// cancel ends it; the beats go on until Work returns.
	jctx, cancel := context.WithCancel(t.Context())
	err := h.Work(jctx, job, func(context.Context) error {
		atStart = store.count()
		cancel()
		deadline := time.Now().Add(time.Second)
		for store.count() < 4 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if atStart != 1 {
		t.Fatalf("beats before the job started = %d, want the first one", atStart)
	}
	after := store.count()
	if after < 4 {
		t.Fatalf("beats while the job ran = %d, want at least 4", after)
	}
	time.Sleep(30 * time.Millisecond)
	if got := store.count(); got != after {
		t.Fatalf("beats after Work returned = %d, want none past %d", got, after)
	}
	for _, id := range store.beats {
		if id != 42 {
			t.Fatalf("beat for job %d, want 42", id)
		}
	}
}

func TestJobHeartbeatRefusesToStartUnseen(t *testing.T) {
	store := &fakeHeartbeater{fail: 1}
	h := &JobHeartbeat{Store: store, Logger: slog.New(slog.DiscardHandler), every: time.Millisecond}
	ran := false
	err := h.Work(t.Context(), &rivertype.JobRow{ID: 7}, func(context.Context) error {
		ran = true
		return nil
	})
	if err == nil || ran {
		t.Fatalf("Work = %v, ran = %v; want an error and the job not run", err, ran)
	}
	if store.count() != 0 {
		t.Fatalf("beats = %d, want none", store.count())
	}
}
