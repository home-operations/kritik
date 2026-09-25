package worker

import (
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/agent"
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
