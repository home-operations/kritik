package worker

import (
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
)

// Timeout implements river.Worker: the runner's deadline, the agent's in
// agentic mode, plus the lease wait and the publish phase around it.
func (w *Review) Timeout(job *river.Job[jobs.ReviewArgs]) time.Duration {
	file := w.Current.Get()
	tenant := tenantByID(file, job.Args.TenantID)
	deadline, _ := file.RunnerFor(tenant)
	if tenant != nil {
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
	file := w.Current.Get()
	deadline, _ := file.RunnerFor(tenantByID(file, job.Args.TenantID))
	return min(deadline+jobtimeout.IndexWriteHeadroom, jobtimeout.MaxJobTimeout)
}

// Timeout implements river.Worker: the lease wait, model call and forge
// write-back a follow-up reply makes, capped like every other job kind.
func (w *FollowUp) Timeout(*river.Job[jobs.FollowUpArgs]) time.Duration {
	return min(jobtimeout.FollowUpTimeout, jobtimeout.MaxJobTimeout)
}

// Timeout implements river.Worker: a task waits for a lease, calls the
// model and writes back like a follow-up.
func (w *Task) Timeout(*river.Job[jobs.TaskArgs]) time.Duration {
	return min(jobtimeout.FollowUpTimeout, jobtimeout.MaxJobTimeout)
}

// repoSettings resolves a repository's settings from its id, which a job
// carries instead of the installation and name the configuration is keyed
// by. A repository the tenant does not list gets the tenant's settings, as
// in Settings.
func repoSettings(file *configfile.File, tenant *configfile.Tenant, repositoryID string) configfile.Settings {
	for i := range tenant.Repositories {
		r := &tenant.Repositories[i]
		if in := file.InstallationFor(tenant, r); in != nil && configfile.RepositoryID(in.ID(), r.Name) == repositoryID {
			return file.Settings(tenant, in.Name, r.Name)
		}
	}
	return file.Settings(tenant, "", "")
}
