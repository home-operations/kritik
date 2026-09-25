package executor

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
)

// Local runs the runner in this process against the given store, which must
// be opened with the runner role's DSN. It exists for tests and local
// development; it is not a supported deployment mode, because it gives the
// checkout the worker's credentials.
type Local struct {
	Store *store.Store
}

// Run implements Executor.
func (l *Local) Run(ctx context.Context, spec Spec) Result {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	started := time.Now()
	if spec.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Deadline)
		defer cancel()
	}
	err := runner.Run(ctx, l.Store, spec.Job, spec.Secrets, logger)
	logTail := tail(spec.Secrets.Mask(buf.String()), LogTailBytes)
	res := Result{JobName: "local", PodName: "local", StartedAt: started, LogTail: logTail, Err: err}
	if err != nil {
		res.ExitCode = 1
		res.TerminationReason = "Error"
		if ctx.Err() == context.DeadlineExceeded {
			res.DeadlineExceeded = true
			res.TerminationReason = "DeadlineExceeded"
		}
	}
	return res
}

// LogTailBytes is how much of a runner's output is kept: enough to diagnose,
// small enough to store per run.
const LogTailBytes = 64 << 10

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
