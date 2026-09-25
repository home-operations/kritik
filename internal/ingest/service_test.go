package ingest

import (
	"testing"
	"time"

	"github.com/riverqueue/river"
)

func TestReviewInsertOpts(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		trigger string
		settle  time.Duration
		want    *river.InsertOpts
	}{
		{"opened is immediate", "opened", time.Minute, nil},
		{"synchronize with no settle is immediate", "synchronize", 0, nil},
		{"synchronize with settle is delayed", "synchronize", time.Minute, &river.InsertOpts{ScheduledAt: now.Add(time.Minute)}},
		{"poll with settle is delayed", "poll", time.Minute, &river.InsertOpts{ScheduledAt: now.Add(time.Minute)}},
		{"reopened is immediate", "reopened", time.Minute, nil},
		{"ready_for_review is immediate", "ready_for_review", time.Minute, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewInsertOpts(tt.trigger, tt.settle, now)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
			if got != nil && !got.ScheduledAt.Equal(tt.want.ScheduledAt) {
				t.Fatalf("ScheduledAt = %v, want %v", got.ScheduledAt, tt.want.ScheduledAt)
			}
		})
	}
}
