package worker

import (
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
)

// River cancels a job's context once its timeout passes, and supervise
// deletes the runner Job when that happens, so a job's timeout must cover
// everything the worker does around the runner as well as the runner.
const (
	// leaseWaitHeadroom is the time a review may spend waiting for a model
	// lease, before an agentic runner or before the worker's model call.
	leaseWaitHeadroom = 15 * time.Minute
	// publishHeadroom covers the worker's side of a review after the
	// runner: reading the pack, the model call in single mode, embedding
	// for similar code and the forge write-back.
	publishHeadroom = 15 * time.Minute
	// indexWriteHeadroom covers embedding a repository's staged chunks and
	// swapping the generation after the index runner ends.
	indexWriteHeadroom = 45 * time.Minute
	// MaxJobTimeout bounds any review or index job, so a runner deadline
	// set absurdly high cannot hold a worker slot for longer than the
	// stuck-job rescuer waits.
	MaxJobTimeout = 3 * time.Hour
	// RescueStuckJobsAfter is how long a running job is left alone before
	// River treats it as abandoned by a dead worker. It must exceed
	// MaxJobTimeout, or a job still working would be run a second time.
	RescueStuckJobsAfter = MaxJobTimeout + time.Hour
)

// Timeout implements river.Worker: the runner's deadline, the agent's in
// agentic mode, plus the lease wait and the publish phase around it.
func (w *Review) Timeout(job *river.Job[jobs.ReviewArgs]) time.Duration {
	file := w.Current.Get()
	tenant := tenantByID(file, job.Args.TenantID)
	deadline := w.Deadline
	if tenant != nil {
		deadline, _ = runnerSpec(tenant, w.Deadline)
		settings := repoSettings(file, tenant, job.Args.RepositoryID)
		if settings.Mode == configfile.ReviewAgentic {
			deadline = agentDeadline(deadline, settings.Agent.Timeout)
		}
	}
	return min(deadline+leaseWaitHeadroom+publishHeadroom, MaxJobTimeout)
}

// Timeout implements river.Worker: the runner's deadline plus embedding
// and writing the generation, which waits on embedding leases.
func (w *Index) Timeout(job *river.Job[jobs.IndexArgs]) time.Duration {
	deadline := w.Deadline
	if tenant := tenantByID(w.Current.Get(), job.Args.TenantID); tenant != nil {
		deadline, _ = runnerSpec(tenant, w.Deadline)
	}
	return min(deadline+indexWriteHeadroom, MaxJobTimeout)
}

// repoSettings resolves a repository's settings from its id, which a job
// carries instead of the name the configuration is keyed by. A repository
// the tenant does not list gets the tenant's settings, as in Settings.
func repoSettings(file *configfile.File, tenant *configfile.Tenant, repositoryID string) configfile.Settings {
	for i := range tenant.Installations {
		inID := tenant.Installations[i].ID()
		for _, r := range tenant.Repositories {
			if configfile.RepositoryID(inID, r.Name) == repositoryID {
				return file.Settings(tenant, r.Name)
			}
		}
	}
	return file.Settings(tenant, "")
}
