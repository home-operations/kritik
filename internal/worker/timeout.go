package worker

import (
	"slices"
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
// model and writes back like a follow-up, or, where the repository allows
// agentic tasks, runs its runner as long as an agentic review may. The job
// does not know its task's mode until it reads the task's definition, so
// the longer bound applies to every task there.
func (w *Task) Timeout(job *river.Job[jobs.TaskArgs]) time.Duration {
	if w.timeout > 0 {
		return w.timeout
	}
	timeout := jobtimeout.FollowUpTimeout
	file := w.Current.Get()
	if tenant := tenantByID(file, job.Args.TenantID); tenant != nil && w.GatewayURL != "" {
		settings := repoSettings(file, tenant, job.Args.RepositoryID)
		modes := settings.Allow.Modes
		if modes == nil {
			modes = []configfile.ReviewMode{settings.Mode}
		}
		if slices.Contains(modes, configfile.ReviewAgentic) {
			agentTimeout := settings.Agent.Timeout
			if t := settings.Allow.Agent.Timeout; t != nil {
				agentTimeout = max(agentTimeout, *t)
			}
			deadline, _ := file.RunnerFor(tenant)
			timeout = max(timeout, agentDeadline(deadline, agentTimeout)+jobtimeout.LeaseWaitHeadroom+jobtimeout.PublishHeadroom)
		}
	}
	return min(timeout, jobtimeout.MaxJobTimeout)
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
