// Package jobtimeout holds the River job timeout bounds shared between the
// worker role, which computes each job's timeout, and configfile, which
// rejects operator settings that would make River's cap cut a job short.
// configfile must not import worker, so these constants live in this leaf
// package instead, which neither worker nor configfile's other leaf
// dependencies import back.
package jobtimeout

import "time"

// River cancels a job's context once its timeout passes, and supervise
// deletes the runner Job when that happens, so a job's timeout must cover
// everything the worker does around the runner as well as the runner.
const (
	// LeaseWaitHeadroom is the time a review may spend waiting for a model
	// lease, before an agentic runner or before the worker's model call.
	LeaseWaitHeadroom = 15 * time.Minute
	// PublishHeadroom covers the worker's side of a review after the
	// runner: reading the pack, the model call in single mode, embedding
	// for similar code and the forge write-back.
	PublishHeadroom = 15 * time.Minute
	// IndexWriteHeadroom covers embedding a repository's staged chunks and
	// swapping the generation after the index runner ends; the embedding
	// pass of a large repository is many model calls.
	IndexWriteHeadroom = 60 * time.Minute
	// AgentFetchHeadroom is the job time an agentic run keeps for fetching
	// and the context pack on top of the agent's own timeout.
	AgentFetchHeadroom = 5 * time.Minute
	// FollowUpTimeout is the time a follow-up job may spend waiting for a
	// model lease, calling the model, and writing its reply back to the
	// forge; unlike Review and Index it has no runner deadline to add to.
	FollowUpTimeout = 30 * time.Minute
	// MaxJobTimeout bounds any review or index job, so a runner deadline
	// set absurdly high cannot hold a worker slot for longer than the
	// stuck-job rescuer waits.
	MaxJobTimeout = 3 * time.Hour
	// RescueStuckJobsAfter is how long a running job is left alone before
	// River treats it as abandoned by a dead worker. It must exceed
	// MaxJobTimeout, or a job still working would be run a second time.
	RescueStuckJobsAfter = MaxJobTimeout + time.Hour
)

// MaxRunnerDeadline is the largest runner.activeDeadlineSeconds a tenant may
// set. Past it, either a single-mode review's timeout (deadline plus
// LeaseWaitHeadroom and PublishHeadroom) or an index's timeout (deadline
// plus IndexWriteHeadroom) would exceed MaxJobTimeout, and River would cut
// the runner off before it finishes rather than the deadline doing so.
const MaxRunnerDeadline = min(MaxJobTimeout-LeaseWaitHeadroom-PublishHeadroom, MaxJobTimeout-IndexWriteHeadroom)

// MaxAgentTimeout is the largest agent.timeout a repository in agentic mode
// may set. Past it, agentDeadline's own contribution (agent.timeout plus
// AgentFetchHeadroom, LeaseWaitHeadroom and PublishHeadroom) alone would
// exceed MaxJobTimeout, regardless of the tenant's runner deadline.
const MaxAgentTimeout = MaxJobTimeout - AgentFetchHeadroom - LeaseWaitHeadroom - PublishHeadroom
