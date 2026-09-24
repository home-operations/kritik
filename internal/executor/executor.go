// Package executor runs a runner for one review: as a Kubernetes Job in
// production, in-process for tests and local development. The worker owns
// the run row either way; the executor reports how the run ended.
package executor

import (
	"context"
	"time"

	"github.com/home-operations/kritik/internal/runner"
)

// Spec is one runner invocation.
type Spec struct {
	// RunID is the runner_runs row; it becomes the Job name suffix and the
	// runner's KRITIK_RUN_ID.
	RunID string
	// Labels are put on the Job for kubectl and Grafana: tenant, repository,
	// pr, kind.
	Labels map[string]string
	// Annotations carry the River job id and head SHA.
	Annotations map[string]string
	// Params are handed to the runner as environment.
	Params runner.Params
	// Deadline bounds the whole run.
	Deadline time.Duration
	// Resources overrides the pod's resource requirements, as the tenant's
	// runner block in the configuration file spells them.
	Resources map[string]any
}

// Result is how a run ended, as far as the executor can tell.
type Result struct {
	JobName, PodName, NodeName string
	ScheduledAt, StartedAt     time.Time
	ExitCode                   int
	TerminationReason          string
	DeadlineExceeded           bool
	LogTail                    string
	// Err is set when the run did not complete successfully.
	Err error
}

// Executor runs a spec to completion.
type Executor interface {
	Run(ctx context.Context, spec Spec) Result
}
