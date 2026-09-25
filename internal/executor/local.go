package executor

import (
	"bytes"
	"context"
	"fmt"
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
	// The spec goes through the same encoding and strict decoding as a
	// Job's mounted document, size limit included.
	job, err := specRoundTrip(spec.Job)
	if err == nil {
		err = runner.Run(ctx, l.Store, job, spec.Secrets, logger)
	}
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

func specRoundTrip(s runner.Spec) (runner.Spec, error) {
	b, err := runner.EncodeSpec(s)
	if err != nil {
		return runner.Spec{}, fmt.Errorf("executor: %w", err)
	}
	return runner.DecodeSpec(b)
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
