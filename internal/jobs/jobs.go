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

// ReviewArgs reviews one head of one pull request.
type ReviewArgs struct {
	TenantID     string `json:"tenant_id"     river:"unique"`
	RepositoryID string `json:"repository_id" river:"unique"`
	Number       int    `json:"number"        river:"unique"`
	HeadSHA      string `json:"head_sha"      river:"unique"`
	// Trigger is why: opened, synchronize, reopened, ready_for_review, poll.
	Trigger string `json:"trigger"`
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

// IndexArgs builds or advances a repository's index to a commit.
type IndexArgs struct {
	TenantID     string `json:"tenant_id"`
	RepositoryID string `json:"repository_id" river:"unique"`
	CommitSHA    string `json:"commit_sha"    river:"unique"`
	// Trigger is why: onboard, push, reindex.
	Trigger string `json:"trigger"`
}

// Kind implements river.JobArgs.
func (IndexArgs) Kind() string { return "index" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (IndexArgs) InsertOpts() river.InsertOpts {
	// Unique while queued or running only: the same commit may be indexed
	// again later (an onboarding job that could not run, a rebuild), and
	// the worker skips a commit the active generation already has.
	return river.InsertOpts{Queue: QueueIndex, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
		rivertype.JobStateRunning, rivertype.JobStateScheduled,
	}}}
}
