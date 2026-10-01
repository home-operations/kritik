package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/home-operations/kritika/internal/store"
)

// fakeRescueStore hands out abandoned jobs and orphaned runs and records
// what the Rescuer did with them.
type fakeRescueStore struct {
	abandoned []store.AbandonedJob
	orphaned  []store.OrphanedRun
	// rescued says which job ids RescueJob reports as handed back.
	rescued map[int64]bool
	// endErr fails EndOrphanedRun for the run id.
	endErr string

	rescues, revoked, ended []string
	swept                   int
}

func (f *fakeRescueStore) AbandonedJobs(context.Context, time.Duration, int) ([]store.AbandonedJob, error) {
	return f.abandoned, nil
}

func (f *fakeRescueStore) RescueJob(_ context.Context, job store.AbandonedJob, _ time.Duration, reason string) (bool, error) {
	if reason != rescueReason {
		return false, errors.New("unexpected reason")
	}
	f.rescues = append(f.rescues, string(job.RescueState()))
	return f.rescued[job.ID], nil
}

func (f *fakeRescueStore) OrphanedRuns(context.Context, int) ([]store.OrphanedRun, error) {
	return f.orphaned, nil
}

func (f *fakeRescueStore) RevokeGatewayTokens(_ context.Context, runID string) error {
	f.revoked = append(f.revoked, runID)
	return nil
}

func (f *fakeRescueStore) EndOrphanedRun(_ context.Context, run store.OrphanedRun, _ string) error {
	if run.ID == f.endErr {
		return errors.New("end failed")
	}
	f.ended = append(f.ended, run.ID)
	return nil
}

func (f *fakeRescueStore) SweepJobHeartbeats(context.Context) (int64, error) {
	f.swept++
	return 0, nil
}

// fakeRunDeleter deletes runs' Jobs, refusing the one in fail.
type fakeRunDeleter struct {
	deleted []string
	fail    string
}

func (f *fakeRunDeleter) DeleteRun(_ context.Context, runID string) error {
	if runID == f.fail {
		return errors.New("api server down")
	}
	f.deleted = append(f.deleted, runID)
	return nil
}

func TestRescueState(t *testing.T) {
	tests := []struct {
		name string
		job  store.AbandonedJob
		want rivertype.JobState
	}{
		{name: "attempts left", job: store.AbandonedJob{Attempt: 1, MaxAttempts: 3}, want: rivertype.JobStateRetryable},
		{name: "last attempt", job: store.AbandonedJob{Attempt: 3, MaxAttempts: 3}, want: rivertype.JobStateDiscarded},
		{name: "cancel asked", job: store.AbandonedJob{Attempt: 1, MaxAttempts: 3, CancelRequested: true}, want: rivertype.JobStateCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.RescueState(); got != tt.want {
				t.Errorf("RescueState() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRescue(t *testing.T) {
	jobs := []store.AbandonedJob{{ID: 1, Kind: "review", Attempt: 1, MaxAttempts: 25}, {ID: 2, Kind: "index", Attempt: 3, MaxAttempts: 3}}
	runs := []store.OrphanedRun{
		{ID: "run-a", Kind: store.RunnerKindReview, ReviewID: "rev-a"},
		{ID: "run-b", Kind: store.RunnerKindIndex, IndexRunID: "idx-b"},
	}
	tests := []struct {
		name            string
		st              *fakeRescueStore
		runs            *fakeRunDeleter
		wantErr         bool
		wantRescues     []string
		wantDeleted     []string
		wantRevoked     []string
		wantEnded       []string
		wantNoRunDelete bool
	}{
		{
			name:        "jobs handed back and their runs reaped",
			st:          &fakeRescueStore{abandoned: jobs, orphaned: runs, rescued: map[int64]bool{1: true, 2: true}},
			runs:        &fakeRunDeleter{},
			wantRescues: []string{"retryable", "discarded"},
			wantDeleted: []string{"run-a", "run-b"}, wantRevoked: []string{"run-a", "run-b"}, wantEnded: []string{"run-a", "run-b"},
		},
		{
			name:        "a job that finished meanwhile is left alone",
			st:          &fakeRescueStore{abandoned: jobs[:1], rescued: map[int64]bool{}},
			runs:        &fakeRunDeleter{},
			wantRescues: []string{"retryable"},
		},
		{
			name:        "a Job the API server will not delete keeps its run open for next time",
			st:          &fakeRescueStore{orphaned: runs},
			runs:        &fakeRunDeleter{fail: "run-a"},
			wantErr:     true,
			wantDeleted: []string{"run-b"}, wantRevoked: []string{"run-b"}, wantEnded: []string{"run-b"},
		},
		{
			name:    "a run that cannot be ended is still deleted and revoked",
			st:      &fakeRescueStore{orphaned: runs[:1], endErr: "run-a"},
			runs:    &fakeRunDeleter{},
			wantErr: true, wantDeleted: []string{"run-a"}, wantRevoked: []string{"run-a"},
		},
		{
			name:        "without a Kubernetes executor runs are ended all the same",
			st:          &fakeRescueStore{orphaned: runs},
			wantRevoked: []string{"run-a", "run-b"}, wantEnded: []string{"run-a", "run-b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Rescuer{Store: tt.st, Logger: slog.New(slog.DiscardHandler)}
			if tt.runs != nil {
				r.Runs = tt.runs
			}
			err := r.Rescue(t.Context())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Rescue() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(tt.st.rescues, tt.wantRescues) {
				t.Errorf("rescues = %v, want %v", tt.st.rescues, tt.wantRescues)
			}
			if tt.runs != nil && !slices.Equal(tt.runs.deleted, tt.wantDeleted) {
				t.Errorf("deleted = %v, want %v", tt.runs.deleted, tt.wantDeleted)
			}
			if !slices.Equal(tt.st.revoked, tt.wantRevoked) {
				t.Errorf("revoked = %v, want %v", tt.st.revoked, tt.wantRevoked)
			}
			if !slices.Equal(tt.st.ended, tt.wantEnded) {
				t.Errorf("ended = %v, want %v", tt.st.ended, tt.wantEnded)
			}
			if tt.st.swept != 1 {
				t.Errorf("heartbeat sweeps = %d, want 1", tt.st.swept)
			}
		})
	}
}
