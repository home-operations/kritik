package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/executor"
)

// fakeState is a run as the database would report it, changed by the test
// while the fake executor blocks.
type fakeState struct {
	mu      sync.Mutex
	started bool
	age     time.Duration
	head    string
}

func (f *fakeState) heartbeat(context.Context) (time.Duration, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.age, f.started, nil
}

func (f *fakeState) headSHA(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

func (f *fakeState) set(fn func(*fakeState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// blockingExecutor runs until its context ends, as a Job would until the
// executor deletes it.
type blockingExecutor struct{ started chan struct{} }

func (b *blockingExecutor) Run(ctx context.Context, _ executor.Spec) executor.Result {
	close(b.started)
	<-ctx.Done()
	return executor.Result{JobName: "kritik-run-x", Err: context.Cause(ctx)}
}

type instantExecutor struct{}

func (instantExecutor) Run(context.Context, executor.Spec) executor.Result {
	return executor.Result{JobName: "kritik-run-x"}
}

func TestSupervise(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name      string
		checkHead bool
		exec      executor.Executor
		// change is applied once the run has started.
		change    func(*fakeState)
		wantCause error
	}{
		{name: "normal completion is untouched", checkHead: true, exec: instantExecutor{}},
		{name: "stale heartbeat after start is lost", exec: &blockingExecutor{started: make(chan struct{})},
			change: func(f *fakeState) { f.started, f.age = true, 2*time.Minute }, wantCause: errHeartbeatLost},
		{name: "head moves while running is superseded", checkHead: true, exec: &blockingExecutor{started: make(chan struct{})},
			change: func(f *fakeState) { f.head = "89abcdef0123456789abcdef0123456789abcdef" }, wantCause: errSuperseded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &fakeState{head: head}
			s := supervision{
				every: 5 * time.Millisecond, stale: 90 * time.Second, heartbeat: state.heartbeat,
				logger: slog.New(slog.DiscardHandler),
			}
			if tt.checkHead {
				s.head, s.want = state.headSHA, head
			}
			if b, ok := tt.exec.(*blockingExecutor); ok {
				go func() {
					<-b.started
					// A fresh heartbeat before start must not end the run.
					time.Sleep(20 * time.Millisecond)
					state.set(tt.change)
				}()
			}
			done := make(chan struct{})
			var res executor.Result
			var cause error
			go func() {
				res, cause = supervise(t.Context(), s, tt.exec, executor.Spec{})
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("supervise did not return")
			}
			if !errors.Is(cause, tt.wantCause) {
				t.Fatalf("cause = %v, want %v", cause, tt.wantCause)
			}
			if tt.wantCause == nil && res.Err != nil {
				t.Fatalf("result err = %v", res.Err)
			}
			if tt.wantCause != nil && !errors.Is(res.Err, tt.wantCause) {
				t.Fatalf("the executor must see the cause, got %v", res.Err)
			}
		})
	}
}

func TestSuperviseLeavesParentCancellation(t *testing.T) {
	state := &fakeState{}
	ctx, cancel := context.WithCancel(t.Context())
	b := &blockingExecutor{started: make(chan struct{})}
	go func() { <-b.started; cancel() }()
	res, cause := supervise(ctx, supervision{every: time.Hour, stale: time.Minute, heartbeat: state.heartbeat, logger: slog.New(slog.DiscardHandler)}, b, executor.Spec{})
	if cause != nil || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("cause = %v, err = %v", cause, res.Err)
	}
}
