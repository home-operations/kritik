package worker

import (
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
)

// MaxJobTimeout and RescueStuckJobsAfter are re-exported from jobtimeout for
// callers that only import worker (e.g. cmd/kritik's river.Config).
const (
	MaxJobTimeout        = jobtimeout.MaxJobTimeout
	RescueStuckJobsAfter = jobtimeout.RescueStuckJobsAfter
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
	return min(deadline+jobtimeout.LeaseWaitHeadroom+jobtimeout.PublishHeadroom, jobtimeout.MaxJobTimeout)
}

// Timeout implements river.Worker: the runner's deadline plus embedding
// and writing the generation, which waits on embedding leases.
func (w *Index) Timeout(job *river.Job[jobs.IndexArgs]) time.Duration {
	deadline := w.Deadline
	if tenant := tenantByID(w.Current.Get(), job.Args.TenantID); tenant != nil {
		deadline, _ = runnerSpec(tenant, w.Deadline)
	}
	return min(deadline+jobtimeout.IndexWriteHeadroom, jobtimeout.MaxJobTimeout)
}

// Timeout implements river.Worker: the lease wait, model call and forge
// write-back a follow-up reply makes, capped like every other job kind.
func (w *FollowUp) Timeout(*river.Job[jobs.FollowUpArgs]) time.Duration {
	return min(jobtimeout.FollowUpTimeout, jobtimeout.MaxJobTimeout)
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
