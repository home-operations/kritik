package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/transcript"
)

// FindingRow is one findings row.
type FindingRow struct {
	ID             string
	Path           string
	Line           int
	EndLine        int
	Severity       review.Severity
	Title          string
	Explanation    string
	SuggestedFix   string
	Replacement    string
	AgentPrompt    string
	Fingerprint    string
	PostedInline   bool
	ForgeCommentID *int64
	CreatedAt      time.Time
}

// ListFindings returns a review's findings, most serious first.
func ListFindings(ctx context.Context, tx pgx.Tx, reviewID string) ([]FindingRow, error) {
	rows, err := tx.Query(ctx, `SELECT id, path, line, end_line, severity, title, explanation, suggested_fix, replacement,
		agent_prompt, fingerprint, posted_inline, forge_comment_id, created_at
		FROM findings WHERE review_id = $1
		ORDER BY CASE severity WHEN 'blocking' THEN 0 WHEN 'important' THEN 1 ELSE 2 END, path, line, id`, reviewID)
	if err != nil {
		return nil, fmt.Errorf("store: list findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (FindingRow, error) {
		var f FindingRow
		var sev string
		err := row.Scan(&f.ID, &f.Path, &f.Line, &f.EndLine, &sev, &f.Title, &f.Explanation, &f.SuggestedFix, &f.Replacement,
			&f.AgentPrompt, &f.Fingerprint, &f.PostedInline, &f.ForgeCommentID, &f.CreatedAt)
		f.Severity = review.Severity(sev)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list findings: %w", err)
	}
	return out, nil
}

// RunnerRunRow is one runner_runs row.
type RunnerRunRow struct {
	ID                string
	Kind              string
	JobName           string
	PodName           string
	NodeName          string
	Phase             string
	CreatedAt         time.Time
	ScheduledAt       *time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	HeartbeatAt       *time.Time
	ExitCode          *int
	TerminationReason string
	DeadlineExceeded  bool
	LogTail           string
	Error             string
}

// LatestRunnerRun returns the newest runner run that prepared a review, or
// ErrNotFound when none did.
func LatestRunnerRun(ctx context.Context, tx pgx.Tx, reviewID string) (RunnerRunRow, error) {
	var r RunnerRunRow
	err := tx.QueryRow(ctx, `SELECT id, kind, job_name, pod_name, node_name, phase, created_at, scheduled_at, started_at,
		finished_at, heartbeat_at, exit_code, termination_reason, deadline_exceeded, log_tail, error
		FROM runner_runs WHERE review_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, reviewID).
		Scan(&r.ID, &r.Kind, &r.JobName, &r.PodName, &r.NodeName, &r.Phase, &r.CreatedAt, &r.ScheduledAt, &r.StartedAt,
			&r.FinishedAt, &r.HeartbeatAt, &r.ExitCode, &r.TerminationReason, &r.DeadlineExceeded, &r.LogTail, &r.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("store: latest runner run: %w", err)
	}
	return r, nil
}

// TimelineStep is one agent step as agent_runs.timeline records it.
type TimelineStep struct {
	Index        int      `json:"index"`
	Tools        []string `json:"tools"`
	DurationMS   int64    `json:"duration_ms"`
	OutputBytes  int      `json:"output_bytes"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
}

// AgentRunRow is one agent_runs row.
type AgentRunRow struct {
	StopReason string
	// Result is the submitted review JSON, nil unless the agent submitted.
	Result    json.RawMessage
	Steps     int
	ToolCalls map[string]int
	Timeline  []TimelineStep
	Sources   []string
	Usage     model.Usage
	CostUSD   float64
	Model     string
	Error     string
	CreatedAt time.Time
}

// FindAgentRun returns the agent run of a runner run, or ErrNotFound.
func FindAgentRun(ctx context.Context, tx pgx.Tx, runnerRunID string) (AgentRunRow, error) {
	var a AgentRunRow
	var result, calls, timeline, sources []byte
	err := tx.QueryRow(ctx, `SELECT stop_reason, result, steps, tool_calls, timeline, sources, input_tokens, cache_read_tokens,
		cache_write_tokens, output_tokens, cost_usd::float8, model, error, created_at FROM agent_runs WHERE runner_run_id = $1`, runnerRunID).
		Scan(&a.StopReason, &result, &a.Steps, &calls, &timeline, &sources, &a.Usage.Input, &a.Usage.CacheRead,
			&a.Usage.CacheWrite, &a.Usage.Output, &a.CostUSD, &a.Model, &a.Error, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("store: find agent run: %w", err)
	}
	if len(result) > 0 {
		a.Result = json.RawMessage(result)
	}
	a.ToolCalls, a.Timeline, a.Sources = map[string]int{}, []TimelineStep{}, []string{}
	for _, d := range []struct {
		raw []byte
		v   any
	}{{calls, &a.ToolCalls}, {timeline, &a.Timeline}, {sources, &a.Sources}} {
		if err := json.Unmarshal(d.raw, d.v); err != nil {
			return a, fmt.Errorf("store: decode agent run: %w", err)
		}
	}
	return a, nil
}

// UsageRow is one usage row.
type UsageRow struct {
	Role         string
	Model        string
	Upstream     string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	CreatedAt    time.Time
}

// ListReviewUsage returns the usage rows charged to a review, oldest first.
func ListReviewUsage(ctx context.Context, tx pgx.Tx, reviewID string) ([]UsageRow, error) {
	rows, err := tx.Query(ctx, `SELECT role, model, upstream, input_tokens, output_tokens, cost_usd::float8, created_at
		FROM usage WHERE review_id = $1 ORDER BY created_at, id`, reviewID)
	if err != nil {
		return nil, fmt.Errorf("store: list review usage: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageRow, error) {
		var u UsageRow
		err := row.Scan(&u.Role, &u.Model, &u.Upstream, &u.InputTokens, &u.OutputTokens, &u.CostUSD, &u.CreatedAt)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list review usage: %w", err)
	}
	return out, nil
}

// ContextPackMeta is a context pack without its diffs, stage texts or file
// contents, which the database leaves out rather than sending.
type ContextPackMeta struct {
	HeadSHA      string
	BaseSHA      string
	PatchID      string
	ChangedPaths []string
	DeltaPaths   []string
	PriorHeadSHA *string
	RepoNotes    []string
	// Stages are the context chunks with Text empty; StageBytes[i] is the
	// size of Stages[i]'s text.
	Stages     []contextpack.Chunk
	StageBytes []int
	// RepoFiles maps each repository file the pack read to its size.
	RepoFiles map[string]int
	CreatedAt time.Time
}

// stageMeta is a stage chunk as FindContextPackMeta's query returns it.
type stageMeta struct {
	contextpack.Chunk
	Bytes int `json:"bytes"`
}

// FindContextPackMeta returns the metadata of the context pack a runner run
// wrote, or ErrNotFound.
func FindContextPackMeta(ctx context.Context, tx pgx.Tx, runnerRunID string) (ContextPackMeta, error) {
	var m ContextPackMeta
	var stages, files []byte
	err := tx.QueryRow(ctx, `SELECT head_sha, base_sha, patch_id, changed_paths, delta_paths, prior_head_sha, repo_notes,
		(SELECT coalesce(jsonb_agg((e - 'text') || jsonb_build_object('bytes', octet_length(e->>'text')) ORDER BY n), '[]')
			FROM jsonb_array_elements(stages) WITH ORDINALITY AS s(e, n)),
		(SELECT coalesce(jsonb_object_agg(k, octet_length(v)), '{}') FROM jsonb_each_text(repo_files) AS f(k, v)),
		created_at FROM context_packs WHERE runner_run_id = $1`, runnerRunID).
		Scan(&m.HeadSHA, &m.BaseSHA, &m.PatchID, &m.ChangedPaths, &m.DeltaPaths, &m.PriorHeadSHA, &m.RepoNotes, &stages, &files, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("store: find context pack: %w", err)
	}
	var chunks []stageMeta
	if err := json.Unmarshal(stages, &chunks); err != nil {
		return m, fmt.Errorf("store: decode context stages: %w", err)
	}
	if err := json.Unmarshal(files, &m.RepoFiles); err != nil {
		return m, fmt.Errorf("store: decode repository files: %w", err)
	}
	m.Stages, m.StageBytes = make([]contextpack.Chunk, len(chunks)), make([]int, len(chunks))
	for i, c := range chunks {
		m.Stages[i], m.StageBytes[i] = c.Chunk, c.Bytes
	}
	return m, nil
}

// ContextPackDiffs returns the diff and delta diff of the context pack a
// runner run wrote, or ErrNotFound.
func ContextPackDiffs(ctx context.Context, tx pgx.Tx, runnerRunID string) (diff, delta string, err error) {
	err = tx.QueryRow(ctx, `SELECT diff, delta_diff FROM context_packs WHERE runner_run_id = $1`, runnerRunID).Scan(&diff, &delta)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("store: context pack diffs: %w", err)
	}
	return diff, delta, nil
}

// ContextPackInputs returns the context chunks and repository files of the
// context pack a runner run wrote, or ErrNotFound.
func ContextPackInputs(ctx context.Context, tx pgx.Tx, runnerRunID string) ([]contextpack.Chunk, map[string]string, error) {
	var stages, files []byte
	err := tx.QueryRow(ctx, `SELECT stages, repo_files FROM context_packs WHERE runner_run_id = $1`, runnerRunID).Scan(&stages, &files)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: context pack inputs: %w", err)
	}
	chunks, repoFiles := []contextpack.Chunk{}, map[string]string{}
	if err := json.Unmarshal(stages, &chunks); err != nil {
		return nil, nil, fmt.Errorf("store: decode context stages: %w", err)
	}
	if err := json.Unmarshal(files, &repoFiles); err != nil {
		return nil, nil, fmt.Errorf("store: decode repository files: %w", err)
	}
	return chunks, repoFiles, nil
}

// ReviewModelCalls returns a review's own model calls, in the order they
// were recorded: its agent steps, plain review and fallback calls, not the
// follow-ups later answered against it.
func ReviewModelCalls(ctx context.Context, tx pgx.Tx, reviewID string) ([]transcript.StoredRow, error) {
	return modelCallsWhere(ctx, tx, `review_id = $1::uuid AND kind IN ('agent_step', 'review', 'fallback')`, reviewID)
}

// FollowupModelCalls returns the model calls that answered a follow-up
// comment on one pull request. A comment id is only unique per forge, so
// the pull request pins which comment is meant: a call recorded against a
// review must belong to one of its reviews, and one recorded against no
// review is kept only when no other pull request of the tenant has a
// follow-up with that comment id.
func FollowupModelCalls(ctx context.Context, tx pgx.Tx, pullRequestID string, commentID int64) ([]transcript.StoredRow, error) {
	return modelCallsWhere(ctx, tx, `kind = 'followup' AND followup_comment_id = $2 AND (
		review_id IN (SELECT id FROM reviews WHERE pull_request_id = $1::uuid)
		OR (review_id IS NULL AND NOT EXISTS (SELECT 1 FROM followups
			WHERE comment_id = $2 AND pull_request_id <> $1::uuid)))`, pullRequestID, commentID)
}

func modelCallsWhere(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]transcript.StoredRow, error) {
	rows, err := tx.Query(ctx, `SELECT id, kind, step, coalesce(review_id::text, ''), coalesce(runner_run_id::text, ''),
		coalesce(followup_comment_id, 0), model, upstream, system, tools, messages_from, messages, response,
		input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, cost_usd::float8, duration_ms, error, truncated, created_at
		FROM model_calls WHERE `+where+` ORDER BY created_at, step, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list model calls: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanModelCall)
	if err != nil {
		return nil, fmt.Errorf("store: list model calls: %w", err)
	}
	return out, nil
}
