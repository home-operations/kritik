package worker

import (
	"slices"
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/tasks"
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
// model and writes back like a follow-up, or, where an agentic task may
// run, runs its runner as long as the longest agent timeout such a task
// may have. The job does not know its task until it reads the task's
// definition, so the longer bound applies to every task there.
func (w *Task) Timeout(job *river.Job[jobs.TaskArgs]) time.Duration {
	if w.timeout > 0 {
		return w.timeout
	}
	timeout := jobtimeout.FollowUpTimeout
	file := w.Current.Get()
	if tenant := tenantByID(file, job.Args.TenantID); tenant != nil && w.GatewayURL != "" {
		if agentTimeout, ok := taskAgentTimeout(repoSettings(file, tenant, job.Args.RepositoryID)); ok {
			deadline, _ := file.RunnerFor(tenant)
			timeout = max(timeout, agentDeadline(deadline, agentTimeout)+jobtimeout.LeaseWaitHeadroom+jobtimeout.PublishHeadroom)
		}
	}
	return min(timeout, jobtimeout.MaxJobTimeout)
}

// taskAgentTimeout is the longest agent timeout an agentic task on a
// repository with settings may run with, and whether one may run at all.
// A dashboard's or the repository's task is held to allow.modes and
// allow.agent.timeout (or the operator's own agent timeout); an operator
// file's task is trusted to pick its mode and timeout, so its own count.
func taskAgentTimeout(settings configfile.Settings) (time.Duration, bool) {
	modes := settings.Allow.Modes
	if modes == nil {
		modes = []configfile.ReviewMode{settings.Mode}
	}
	ok := slices.Contains(modes, configfile.ReviewAgentic)
	timeout := settings.Agent.Timeout
	if t := settings.Allow.Agent.Timeout; t != nil && ok {
		timeout = max(timeout, *t)
	}
	for _, t := range settings.Tasks {
		if !t.IsEnabled() || t.RunMode() != tasks.ModeAgentic {
			continue
		}
		ok = true
		if t.Agent.Timeout != nil {
			timeout = max(timeout, *t.Agent.Timeout)
		}
	}
	return timeout, ok
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
