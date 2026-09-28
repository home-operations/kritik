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
// agentic mode, plus the lease wait and the publish phase around it. The
// repository's .kritik.yaml is read only once the job runs, so the timeout
// covers the longest review it may choose: agentic when its bounds allow
// that mode, with the longest agent timeout they allow.
func (w *Review) Timeout(job *river.Job[jobs.ReviewArgs]) time.Duration {
	file := w.Current.Get()
	account := accountByID(file, job.Args.AccountID)
	deadline, _ := file.RunnerFor(account)
	if account != nil {
		settings := repoSettings(file, account, job.Args.RepositoryID)
		if settings.Mode == configfile.ReviewAgentic || slices.Contains(settings.Allow.Modes, configfile.ReviewAgentic) {
			timeout := settings.Agent.Timeout
			if bound := settings.Allow.Agent.Timeout; bound != nil {
				timeout = max(timeout, *bound)
			}
			deadline = agentDeadline(deadline, timeout)
		}
	}
	return min(deadline+jobtimeout.LeaseWaitHeadroom+jobtimeout.PublishHeadroom, jobtimeout.MaxJobTimeout)
}

// Timeout implements river.Worker: the runner's deadline plus embedding
// and writing the generation, which waits on embedding leases.
func (w *Index) Timeout(job *river.Job[jobs.IndexArgs]) time.Duration {
	file := w.Current.Get()
	deadline, _ := file.RunnerFor(accountByID(file, job.Args.AccountID))
	return min(deadline+jobtimeout.IndexWriteHeadroom, jobtimeout.MaxJobTimeout)
}

// Timeout implements river.Worker: the lease wait, model call and forge
// write-back a follow-up reply makes, capped like every other job kind.
func (w *FollowUp) Timeout(*river.Job[jobs.FollowUpArgs]) time.Duration {
	return min(jobtimeout.FollowUpTimeout, jobtimeout.MaxJobTimeout)
}

// repoSettings resolves a repository's settings from its id, which a job
// carries instead of the name the configuration is keyed by. A repository
// the account does not list gets the account's settings, as in Settings.
func repoSettings(file *configfile.File, account *configfile.Account, repositoryID string) configfile.Settings {
	for _, r := range account.Repositories {
		if name := account.Name + "/" + r.Name; configfile.RepositoryID(account.ID(), name) == repositoryID {
			return file.Settings(account, name)
		}
	}
	return file.Settings(account, "")
}
