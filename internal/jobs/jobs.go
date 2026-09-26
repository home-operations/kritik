// Package jobs defines the River job arguments the ingest role enqueues and
// the worker role consumes. Uniqueness lives here because it is the contract
// between the two: a review is unique per head SHA so no push is ever lost,
// a follow-up per comment, an index run per target commit.
package jobs

import (
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue names, one per job kind so the worker can bound each separately.
const (
	QueueReview   = "review"
	QueueFollowUp = "followup"
	QueueIndex    = "index"
)

// TriggerManual is the Trigger a human-requested re-run carries. It is the
// only trigger the worker lets bypass the bot-author patch-id skip.
const TriggerManual = "manual"

// TriggerReindex is the Trigger EnqueueReindex gives a forced full reindex,
// as opposed to the worker-internal onboard and push triggers.
const TriggerReindex = "reindex"

// Index job triggers the service itself sets.
const (
	TriggerOnboard = "onboard"
	TriggerPush    = "push"
)

// Index job priorities, highest first: River always fetches a higher
// priority first, so an onboarding wave never delays keeping an indexed
// repository current.
const (
	indexPriorityUpdate  = 1
	indexPriorityReindex = 2
	indexPriorityOnboard = 4
)

// indexAttempts bounds an index job's tries: transient failures (a fetch
// timeout, a runner killed at its deadline) get River's backoff, and an
// onboarding that fails every try is offered again later.
const indexAttempts = 3

// ReviewArgs reviews one head of one pull request.
type ReviewArgs struct {
	TenantID     string `json:"tenant_id"     river:"unique"`
	RepositoryID string `json:"repository_id" river:"unique"`
	Number       int    `json:"number"        river:"unique"`
	HeadSHA      string `json:"head_sha"      river:"unique"`
	// Trigger is why: opened, synchronize, reopened, ready_for_review, poll,
	// manual.
	Trigger string `json:"trigger"`
	// Request distinguishes one manual re-run from another. River hashes
	// only the river:"unique" fields (sorted by key) to dedupe by args, so
	// the omitempty tag is load-bearing: it must serialize to no "request"
	// key at all (every trigger but manual) for the hash to match what a job
	// enqueued before this field existed would have produced, keeping the
	// existing dedup on tenant+repository+number+head unchanged. A manual
	// re-run sets a fresh value (a UUID) so it is never deduped against a
	// prior run of the same head, including another manual one.
	Request string `json:"request,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (ReviewArgs) Kind() string { return "review" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ReviewArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueReview, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// FollowUpArgs answers one comment that addressed the bot.
type FollowUpArgs struct {
	TenantID     string `json:"tenant_id"`
	RepositoryID string `json:"repository_id"`
	Number       int    `json:"number"`
	CommentID    int64  `json:"comment_id" river:"unique"`
	Inline       bool   `json:"inline"`
	Path         string `json:"path,omitempty"`
	Line         int    `json:"line,omitempty"`
}

// Kind implements river.JobArgs.
func (FollowUpArgs) Kind() string { return "followup" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (FollowUpArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueFollowUp, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// IndexArgs builds or advances a repository's index to its default branch
// tip, whatever the tip is when the job runs.
type IndexArgs struct {
	TenantID     string `json:"tenant_id"`
	RepositoryID string `json:"repository_id" river:"unique"`
	// CommitSHA is the commit a push moved the default branch to, for the
	// record only: a burst of pushes needs one job, not one each, so it is
	// not part of the unique key.
	CommitSHA string `json:"commit_sha"`
	// Trigger is why: TriggerOnboard, TriggerPush or TriggerReindex.
	Trigger string `json:"trigger"`
	// Full forces a full rebuild even when the active generation already
	// covers the tip. It is part of the unique key, so a forced rebuild is
	// queued beside an update rather than folded into it, and dedupes only
	// onto another forced rebuild.
	Full bool `json:"full,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (IndexArgs) Kind() string { return "index" }

// InsertOpts implements river.JobArgsWithInsertOpts. A job is unique per
// repository while queued or running (River requires running in the set):
// a push while the repository's job is running is absorbed by it, and the
// worker indexes the tip again when it finds the branch moved.
func (a IndexArgs) InsertOpts() river.InsertOpts {
	priority := indexPriorityUpdate
	switch a.Trigger {
	case TriggerReindex:
		priority = indexPriorityReindex
	case TriggerOnboard:
		priority = indexPriorityOnboard
	}
	return river.InsertOpts{
		Queue: QueueIndex, Priority: priority, MaxAttempts: indexAttempts,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
			rivertype.JobStateRunning, rivertype.JobStateScheduled,
		}},
	}
}
