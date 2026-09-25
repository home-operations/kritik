package jobtimeout

import (
	"testing"
	"time"
)

func TestBounds(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"max runner deadline", MaxRunnerDeadline, 2 * time.Hour},
		{"index at the max runner deadline", MaxRunnerDeadline + IndexWriteHeadroom, MaxJobTimeout},
		{"max agent timeout", MaxAgentTimeout, 3*time.Hour - 35*time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
	if RescueStuckJobsAfter <= MaxJobTimeout {
		t.Errorf("RescueStuckJobsAfter %v must exceed MaxJobTimeout %v", RescueStuckJobsAfter, MaxJobTimeout)
	}
}
