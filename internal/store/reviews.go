package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

// NewReview is a review the worker starts: its pull request and head, and
// the River job working it.
type NewReview struct {
	AccountID, PullRequestID, HeadSHA, MergeBaseSHA, ForgePatchID, Trigger string
	JobID                                                                  int64
}

// StartReview records a running review and its runner run and returns
// their ids. An earlier attempt of the same job that never finished its
// review (a write that failed and was retried, a worker that died) left it
// running, and a running review blocks every re-run of its head, so it is
// ended failed first.
func StartReview(ctx context.Context, tx pgx.Tx, r NewReview) (reviewID, runID string, err error) {
	if _, err := tx.Exec(ctx, `UPDATE reviews SET status = 'failed', error = 'its job was retried', finished_at = now()
		WHERE pull_request_id = $1 AND river_job_id = $2 AND finished_at IS NULL`, r.PullRequestID, r.JobID); err != nil {
		return "", "", fmt.Errorf("store: end an earlier attempt's review: %w", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO reviews
		(account_id, pull_request_id, head_sha, merge_base_sha, forge_patch_id, status, trigger, river_job_id)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, $7) RETURNING id`,
		r.AccountID, r.PullRequestID, r.HeadSHA, r.MergeBaseSHA, r.ForgePatchID, r.Trigger, r.JobID).Scan(&reviewID); err != nil {
		return "", "", fmt.Errorf("store: insert review: %w", err)
	}
	runID, err = InsertRunnerRun(ctx, tx, r.AccountID, RunnerKindReview, reviewID, r.JobID)
	return reviewID, runID, err
}

// EndedReview is a review that ended before it ran: superseded, skipped or
// refused at admission.
type EndedReview struct {
	AccountID, PullRequestID, HeadSHA, MergeBaseSHA, ForgePatchID, Trigger, Error string
	Status                                                                        ReviewStatus
	SkipReason                                                                    repoconfig.SkipReason
}

// RecordEndedReview records a review that never ran, as its terminal
// status says.
func RecordEndedReview(ctx context.Context, tx pgx.Tx, r EndedReview) error {
	if _, err := tx.Exec(ctx, `INSERT INTO reviews
		(account_id, pull_request_id, head_sha, merge_base_sha, forge_patch_id, status, skip_reason, trigger, error, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`,
		r.AccountID, r.PullRequestID, r.HeadSHA, r.MergeBaseSHA, r.ForgePatchID, r.Status, string(r.SkipReason), r.Trigger, r.Error); err != nil {
		return fmt.Errorf("store: record ended review: %w", err)
	}
	return nil
}

// ReviewEnd is how a review ends: its terminal status and what the row
// records with it. An empty PatchID or SkipReason keeps what the row has.
type ReviewEnd struct {
	Status     ReviewStatus
	PatchID    string
	SkipReason repoconfig.SkipReason
	Error      string
	// OnlyUnfinished ends the review only if nothing has ended it yet.
	OnlyUnfinished bool
}

// EndReview ends the review and reports whether it did: false only when
// end.OnlyUnfinished is set and the review had ended already.
func EndReview(ctx context.Context, tx pgx.Tx, reviewID string, end ReviewEnd) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, patch_id = CASE WHEN $3 = '' THEN patch_id ELSE $3 END,
		skip_reason = CASE WHEN $4 = '' THEN skip_reason ELSE $4 END, error = left($5, 2000), finished_at = now()
		WHERE id = $1 AND (NOT $6 OR finished_at IS NULL)`,
		reviewID, end.Status, end.PatchID, string(end.SkipReason), end.Error, end.OnlyUnfinished)
	if err != nil {
		return false, fmt.Errorf("store: end review: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkReviewPrepared moves a review past its runner: its patch id, what it
// builds on, and the prior review it builds on, "" for none. Prepared is
// not terminal: publishing follows, so finished_at stays NULL.
func MarkReviewPrepared(
	ctx context.Context, tx pgx.Tx, reviewID, patchID string, scope review.Scope, scopeReason, priorReviewID string,
) error {
	if _, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, patch_id = $3, scope = $4, scope_reason = $5,
		prior_review_id = nullif($6, '')::uuid WHERE id = $1`,
		reviewID, ReviewPrepared, patchID, string(scope), scopeReason, priorReviewID); err != nil {
		return fmt.Errorf("store: mark review prepared: %w", err)
	}
	return nil
}

// ReviewJobID is the River job working the review, 0 when none is
// recorded.
func ReviewJobID(ctx context.Context, tx pgx.Tx, reviewID string) (int64, error) {
	var jobID int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(river_job_id, 0) FROM reviews WHERE id = $1`, reviewID).Scan(&jobID); err != nil {
		return 0, fmt.Errorf("store: read review job: %w", err)
	}
	return jobID, nil
}

// RunnerKind is what a runner run is for.
type RunnerKind string

// Runner run kinds, as runner_runs spells them.
const (
	RunnerKindReview RunnerKind = "review"
	RunnerKindIndex  RunnerKind = "index"
)

// InsertRunnerRun records a runner run of kind for its parent, a review or
// an index generation, started by River job jobID, and returns its id.
func InsertRunnerRun(ctx context.Context, tx pgx.Tx, accountID string, kind RunnerKind, parentID string, jobID int64) (string, error) {
	parent := "review_id"
	if kind == RunnerKindIndex {
		parent = "index_run_id"
	}
	var runID string
	if err := tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, `+parent+`, kind, river_job_id) VALUES ($1, $2, $3, $4) RETURNING id`,
		accountID, parentID, string(kind), jobID).Scan(&runID); err != nil {
		return "", fmt.Errorf("store: insert runner run: %w", err)
	}
	return runID, nil
}

// FailRunnerRun ends a runner run that never got a Job, so it does not
// stay 'created' for good.
func FailRunnerRun(ctx context.Context, tx pgx.Tx, runID, errText string) error {
	if _, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = 'failed', error = left($2, 2000), finished_at = now() WHERE id = $1`,
		runID, errText); err != nil {
		return fmt.Errorf("store: fail runner run: %w", err)
	}
	return nil
}

// RunnerResult is what the executor learned about a runner Job.
type RunnerResult struct {
	JobName, PodName, NodeName string
	ScheduledAt, StartedAt     time.Time
	ExitCode                   int
	TerminationReason          string
	DeadlineExceeded           bool
	LogTail                    string
	// Error is why the Job failed, "" when it succeeded.
	Error string
}

// RecordRunnerRun writes what the executor learned about a runner Job. A
// failed Job ends the run failed; a successful one leaves the phase the
// runner set.
func RecordRunnerRun(ctx context.Context, tx pgx.Tx, runID string, r RunnerResult) error {
	if _, err := tx.Exec(ctx, `UPDATE runner_runs SET job_name = $2, pod_name = $3, node_name = $4,
		scheduled_at = nullif($5, '0001-01-01'::timestamptz), started_at = nullif($6, '0001-01-01'::timestamptz), finished_at = now(),
		exit_code = $7, termination_reason = $8, deadline_exceeded = $9, log_tail = $10,
		phase = CASE WHEN $11 THEN phase ELSE 'failed' END, error = CASE WHEN $11 THEN error ELSE left($12, 2000) END
		WHERE id = $1`,
		runID, r.JobName, r.PodName, r.NodeName, r.ScheduledAt, r.StartedAt,
		r.ExitCode, r.TerminationReason, r.DeadlineExceeded, r.LogTail, r.Error == "", r.Error); err != nil {
		return fmt.Errorf("store: record runner run: %w", err)
	}
	return nil
}

// ContextPackRecord is what the worker reads back of a context pack: the
// decisions the runner made, and what it read from the merge base.
type ContextPackRecord struct {
	PatchID, ScopeReason string
	Scope                review.Scope
	// SkipReason is why the runner ran no agent, "" when it did.
	SkipReason string
	// RuleIDs are the rules the prompt was given; Notes what the summary
	// states about the repository's files.
	RuleIDs, Notes []string
	Files          repoconfig.Files
	// StageCounts is how many chunks each stage contributed.
	StageCounts map[string]int
}

// ReadContextPack reads the context pack a runner run wrote, or
// ErrNotFound.
func ReadContextPack(ctx context.Context, tx pgx.Tx, runID string) (ContextPackRecord, error) {
	var rec ContextPackRecord
	var scope string
	var filesJSON, counts []byte
	err := tx.QueryRow(ctx, `SELECT patch_id, skip_reason, scope, scope_reason, rule_ids, repo_notes, repo_files,
		(SELECT coalesce(jsonb_object_agg(stage, n), '{}') FROM (SELECT e->>'stage' AS stage, count(*) AS n
			FROM jsonb_array_elements(stages) AS e GROUP BY 1) AS s)
		FROM context_packs WHERE runner_run_id = $1`, runID).
		Scan(&rec.PatchID, &rec.SkipReason, &scope, &rec.ScopeReason, &rec.RuleIDs, &rec.Notes, &filesJSON, &counts)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, ErrNotFound
	}
	if err != nil {
		return rec, fmt.Errorf("store: read context pack: %w", err)
	}
	rec.Scope = review.Scope(scope)
	if err := json.Unmarshal(filesJSON, &rec.Files); err != nil {
		return rec, fmt.Errorf("store: decode repository files: %w", err)
	}
	if err := json.Unmarshal(counts, &rec.StageCounts); err != nil {
		return rec, fmt.Errorf("store: decode stage counts: %w", err)
	}
	return rec, nil
}

// InlinePosted says whether a finding has an inline comment on the forge,
// and its id there, 0 when the forge did not say.
type InlinePosted struct {
	Posted bool
	ID     int64
}

// ReviewResult is what a published review records: its findings, each with
// whether it is inline on the forge, the summary, the model that answered
// and the sticky comment's id.
type ReviewResult struct {
	AccountID, ReviewID, PullRequestID string
	Result                             review.Result
	Inline                             []InlinePosted
	Model                              string
	CommentID                          int64
}

// RecordReviewResult persists a published review: its findings, the sticky
// comment's id for the pull request, and the review's model and summary.
func RecordReviewResult(ctx context.Context, tx pgx.Tx, r ReviewResult) error {
	for i, f := range r.Result.Findings {
		if _, err := tx.Exec(ctx, `INSERT INTO findings
			(account_id, review_id, path, line, severity, title, explanation, suggested_fix, fingerprint, posted_inline,
			 end_line, replacement, agent_prompt, forge_comment_id, rules)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, nullif($14::bigint, 0), coalesce($15::text[], '{}'))`,
			r.AccountID, r.ReviewID, f.Path, f.Line, string(f.Severity), f.Title, f.Explanation, f.SuggestedFix,
			review.Fingerprint(f), r.Inline[i].Posted, f.EndLine, f.Replacement, f.AgentPrompt, r.Inline[i].ID, f.Rules); err != nil {
			return fmt.Errorf("store: insert finding: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sticky_comments (pull_request_id, account_id, forge_comment_id) VALUES ($1, $2, $3)
		ON CONFLICT (pull_request_id) DO UPDATE SET forge_comment_id = excluded.forge_comment_id, updated_at = now()`,
		r.PullRequestID, r.AccountID, r.CommentID); err != nil {
		return fmt.Errorf("store: upsert sticky comment: %w", err)
	}
	summary, err := json.Marshal(r.Result.Summary)
	if err != nil {
		return fmt.Errorf("store: encode summary: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE reviews SET model = $2, summary = $3 WHERE id = $1`, r.ReviewID, r.Model, summary); err != nil {
		return fmt.Errorf("store: record review model: %w", err)
	}
	return nil
}
