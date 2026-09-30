package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
)

func TestWorkerStopping(t *testing.T) {
	cancelled := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(cause)
		return ctx
	}
	for _, tt := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"a live job", t.Context(), false},
		{"a stopping client's cut", cancelled(errors.New("stop initiated")), true},
		{"an admin's cancel", cancelled(river.ErrJobCancelledRemotely), false},
		{"the job's timeout", cancelled(context.DeadlineExceeded), false},
	} {
		if got := workerStopping(tt.ctx); got != tt.want {
			t.Errorf("%s: workerStopping = %v, want %v", tt.name, got, tt.want)
		}
	}
}
