package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/executor"
)

func TestAgentRunStopError(t *testing.T) {
	tests := []struct {
		name string
		run  agentRun
		want string
	}{
		{name: "submitted", run: agentRun{stop: agent.StopSubmitted, result: []byte(`{}`)}},
		{name: "submitted without a result", run: agentRun{stop: agent.StopSubmitted}, want: "agent stopped: submitted without a result"},
		{name: "max steps", run: agentRun{stop: agent.StopMaxSteps}, want: "agent stopped: max_steps"},
		{name: "budget", run: agentRun{stop: agent.StopBudget}, want: "agent stopped: budget"},
		{name: "no submit", run: agentRun{stop: agent.StopNoSubmit}, want: "agent stopped: no_submit"},
		{name: "model error", run: agentRun{stop: agent.StopError, errText: "model: 500"}, want: "agent stopped: error: model: 500"},
		{name: "timed out", run: agentRun{stop: agent.StopCanceled, errText: "agent timeout (20m0s) reached"},
			want: "agent stopped: canceled: agent timeout (20m0s) reached"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run.stopError()
			if got := errText(err); got != tt.want {
				t.Fatalf("stopError = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentDeadline(t *testing.T) {
	tests := []struct {
		runner, timeout, want time.Duration
	}{
		{runner: 10 * time.Minute, timeout: 20 * time.Minute, want: 25 * time.Minute},
		{runner: time.Hour, timeout: 20 * time.Minute, want: time.Hour},
		{runner: 25 * time.Minute, timeout: 20 * time.Minute, want: 25 * time.Minute},
	}
	for _, tt := range tests {
		if got := agentDeadline(tt.runner, tt.timeout); got != tt.want {
			t.Fatalf("agentDeadline(%s, %s) = %s, want %s", tt.runner, tt.timeout, got, tt.want)
		}
	}
}

func TestStopped(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	failed := executor.Result{Err: errors.New("job failed")}
	tests := []struct {
		name  string
		ctx   context.Context
		res   executor.Result
		cause error
		want  bool
	}{
		{name: "succeeded", ctx: context.Background(), res: executor.Result{}},
		{name: "failed on its own", ctx: context.Background(), res: failed},
		{name: "superseded", ctx: context.Background(), res: failed, cause: errSuperseded, want: true},
		{name: "job context ended", ctx: canceled, res: failed, want: true},
		{name: "deadline", ctx: context.Background(), res: executor.Result{Err: failed.Err, DeadlineExceeded: true}, want: true},
		{name: "finished despite a cancel", ctx: canceled, res: executor.Result{}, cause: errSuperseded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stopped(tt.ctx, tt.res, tt.cause); got != tt.want {
				t.Fatalf("stopped = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentBudget(t *testing.T) {
	tests := []struct {
		name                     string
		agentMax, perMonth, used int64
		want                     int64
		capped                   bool
	}{
		{name: "no monthly cap", agentMax: 4_000_000, used: 9_000_000, want: 4_000_000},
		{name: "plenty left", agentMax: 4_000_000, perMonth: 10_000_000, used: 1_000_000, want: 4_000_000},
		{name: "cut to what is left", agentMax: 4_000_000, perMonth: 10_000_000, used: 9_000_000, want: 1_000_000},
		{name: "repository budget below what is left", agentMax: 200_000, perMonth: 10_000_000, used: 9_000_000, want: 200_000},
		{name: "at the floor", agentMax: 4_000_000, perMonth: 1_000_000, used: 1_000_000 - minAgentTokens, capped: true},
		{name: "over the cap", agentMax: 4_000_000, perMonth: 1_000_000, used: 1_200_000, capped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := agentBudget(tt.agentMax, tt.perMonth, tt.used)
			if got != tt.want || (reason != "") != tt.capped {
				t.Fatalf("agentBudget = %d, %q; want %d, capped %v", got, reason, tt.want, tt.capped)
			}
		})
	}
}
