package webapi

import (
	"testing"

	"github.com/riverqueue/river/rivertype"

	"github.com/home-operations/kritika/internal/store"
)

func TestQueuedMessage(t *testing.T) {
	const timedOut = "github: find App installation for alpha/one: context deadline exceeded"
	for _, tc := range []struct {
		name string
		job  *store.JobRow
		want string
	}{
		{"a running review with no job left", nil, "a review of this head is already queued or running"},
		{"queued", &store.JobRow{ID: 7, State: rivertype.JobStateAvailable, MaxAttempts: 8}, "review job #7 is queued"},
		{"running", &store.JobRow{ID: 7, State: rivertype.JobStateRunning, Attempt: 1, MaxAttempts: 8}, "review job #7 is running"},
		{
			"waiting after the forge did not answer",
			&store.JobRow{ID: 7, State: rivertype.JobStateRetryable, Attempt: 3, MaxAttempts: 8, LastError: timedOut},
			"review job #7 failed attempt 3 of 8 and will run again: GitHub did not answer",
		},
		{
			"waiting after an error with no cause",
			&store.JobRow{ID: 7, State: rivertype.JobStateRetryable, Attempt: 3, MaxAttempts: 8, LastError: "boom"},
			"review job #7 failed attempt 3 of 8 and will run again: boom",
		},
		{
			"running again after the forge did not answer",
			&store.JobRow{ID: 7, State: rivertype.JobStateRunning, Attempt: 4, MaxAttempts: 8, LastError: timedOut},
			"review job #7 is on attempt 4 of 8, the one before failed: GitHub did not answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := queuedMessage(tc.job); got != tc.want {
				t.Errorf("queuedMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}
