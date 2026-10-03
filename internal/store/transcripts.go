package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/transcript"
)

// ModelCallKind is what made a model call; see transcript.Kind.
type ModelCallKind = transcript.Kind

// Model call kinds, as model_calls.kind spells them.
const (
	ModelCallAgentStep = transcript.KindAgentStep
	ModelCallFollowUp  = transcript.KindFollowUp
)

// ModelCall is one row of model_calls. ReviewID, RunnerRunID and
// FollowupCommentID are left empty (zero) when the call has none. Row's
// State is what the next agent step of RunnerRunID is a delta against.
type ModelCall struct {
	AccountID         string
	ReviewID          string
	RunnerRunID       string
	FollowupCommentID int64
	Kind              ModelCallKind
	Step              int
	Model             string
	Upstream          string
	Row               transcript.Encoded
	Stop              model.StopReason
	Usage             model.Usage
	CostUSD           float64
	Duration          time.Duration
	Error             string
}

// InsertModelCall records c in tx, which must be scoped to c's account.
func InsertModelCall(ctx context.Context, tx pgx.Tx, c ModelCall) error {
	if !c.Kind.Valid() {
		return fmt.Errorf("store: model call kind %q", c.Kind)
	}
	var tools *string
	if c.Row.Tools != nil {
		s := string(c.Row.Tools)
		tools = &s
	}
	st := c.Row.State
	_, err := tx.Exec(ctx, `INSERT INTO model_calls
		(account_id, review_id, runner_run_id, followup_comment_id, kind, step, model, upstream, system, tools,
		 messages_from, messages, response, stop_reason, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
		 cost_usd, duration_ms, error, truncated, messages_end, messages_sha, system_sha, tools_sha, run_bytes)
		VALUES ($1, nullif($2, '')::uuid, nullif($3, '')::uuid, nullif($4::bigint, 0), $5, $6, $7, $8, $9, $10::jsonb,
		 $11, $12::jsonb, $13::jsonb, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)`,
		c.AccountID, c.ReviewID, c.RunnerRunID, c.FollowupCommentID, string(c.Kind), c.Step, c.Model, c.Upstream, c.Row.System, tools,
		c.Row.MessagesFrom, string(c.Row.Messages), string(c.Row.Response), string(c.Stop),
		c.Usage.Input, c.Usage.CacheRead, c.Usage.CacheWrite, c.Usage.Output, c.CostUSD, c.Duration.Milliseconds(), c.Error,
		c.Row.Truncated, st.MessagesEnd, st.MessagesSHA[:], st.SystemSHA[:], st.ToolsSHA[:], st.Bytes)
	if err != nil {
		return fmt.Errorf("store: insert model call: %w", err)
	}
	return nil
}

// AgentState returns what the run has recorded so far and the number of
// its next agent step, both zero before its first. It takes a transaction
// lock on the run's transcript, so two steps recorded at once are recorded
// one after the other rather than as deltas against the same state.
func AgentState(ctx context.Context, tx pgx.Tx, runnerRunID string) (transcript.State, int, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kritika-transcript:' || $1, 0))`, runnerRunID); err != nil {
		return transcript.State{}, 0, fmt.Errorf("store: lock transcript: %w", err)
	}
	var st transcript.State
	var step int
	var msgs, system, tools []byte
	err := tx.QueryRow(ctx, `SELECT step, messages_end, messages_sha, system_sha, tools_sha, run_bytes FROM model_calls
		WHERE runner_run_id = $1 AND kind = $2 ORDER BY step DESC LIMIT 1`, runnerRunID, string(ModelCallAgentStep)).
		Scan(&step, &st.MessagesEnd, &msgs, &system, &tools, &st.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return transcript.State{}, 0, nil
	}
	if err != nil {
		return transcript.State{}, 0, fmt.Errorf("store: read transcript state: %w", err)
	}
	copy(st.MessagesSHA[:], msgs)
	copy(st.SystemSHA[:], system)
	copy(st.ToolsSHA[:], tools)
	return st, step + 1, nil
}

func scanModelCall(row pgx.CollectableRow) (transcript.StoredRow, error) {
	var r transcript.StoredRow
	var kind string
	var tools, msgs, resp []byte
	var ms int64
	if err := row.Scan(&r.ID, &kind, &r.Step, &r.ReviewID, &r.RunnerRunID, &r.FollowupCommentID, &r.Model, &r.Upstream, &r.System,
		&tools, &r.MessagesFrom, &msgs, &resp, &r.Usage.Input, &r.Usage.CacheRead, &r.Usage.CacheWrite, &r.Usage.Output,
		&r.CostUSD, &ms, &r.Error, &r.Truncated, &r.CreatedAt); err != nil {
		return r, err
	}
	r.Kind, r.Duration = ModelCallKind(kind), time.Duration(ms)*time.Millisecond
	var err error
	if r.Tools, err = transcript.DecodeTools(tools); err != nil {
		return r, err
	}
	if r.Messages, err = transcript.DecodeMessages(msgs); err != nil {
		return r, err
	}
	r.Response, err = transcript.DecodeResponse(resp)
	return r, err
}

// Rows one statement of a retention sweep takes. A sweep is many such
// statements, each committed on its own, so a backlog too large for the
// owner connection's statement timeout is worked off rather than rolled
// back whole every time. A context pack carries a diff, so its batch is
// the smaller. Variables for the tests.
var (
	modelCallSweepBatch = 5000
	diffSweepBatch      = 200
)

// sweepBatches runs stmt, which sweeps at most $2 rows older than $1
// seconds, until it finds fewer than a batch, and returns how many rows
// were swept, counting those before an error.
func (s *Store) sweepBatches(ctx context.Context, stmt string, olderThan time.Duration, batch int) (int64, error) {
	var total int64
	for {
		tag, err := s.owner.Exec(ctx, stmt, olderThan.Seconds(), batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}

// SweepModelCalls deletes every account's model calls recorded more than
// olderThan ago, a batch at a time, and returns how many it deleted. It
// runs on the owner connection, which row-level security does not
// restrict. Leader only.
func (s *Store) SweepModelCalls(ctx context.Context, olderThan time.Duration) (int64, error) {
	if s.owner == nil {
		return 0, errors.New("store: SweepModelCalls needs the owner connection")
	}
	if olderThan <= 0 {
		return 0, fmt.Errorf("store: model call retention %s is not positive", olderThan)
	}
	n, err := s.sweepBatches(ctx, `DELETE FROM model_calls WHERE id IN (
		SELECT id FROM model_calls WHERE created_at < now() - make_interval(secs => $1) LIMIT $2)`, olderThan, modelCallSweepBatch)
	if err != nil {
		return n, fmt.Errorf("store: sweep model calls: %w", err)
	}
	return n, nil
}

// SweepDiffs empties the bodies of every account's context packs written
// more than olderThan ago, the diffs, the stage texts and the repository
// files, keeping the metadata, a batch at a time, and returns how many
// packs it swept. A swept pack is stamped so the next sweep passes it over.
// Owner connection, leader only, like SweepModelCalls.
func (s *Store) SweepDiffs(ctx context.Context, olderThan time.Duration) (int64, error) {
	if s.owner == nil {
		return 0, errors.New("store: SweepDiffs needs the owner connection")
	}
	if olderThan <= 0 {
		return 0, fmt.Errorf("store: diff retention %s is not positive", olderThan)
	}
	n, err := s.sweepBatches(ctx, `UPDATE context_packs SET diff = '', delta_diff = '', repo_files = '{}'::jsonb, swept_at = now(),
		stages = (SELECT coalesce(jsonb_agg(e - 'text' ORDER BY n), '[]'::jsonb) FROM jsonb_array_elements(stages) WITH ORDINALITY AS s(e, n))
		WHERE runner_run_id IN (SELECT runner_run_id FROM context_packs
			WHERE swept_at IS NULL AND created_at < now() - make_interval(secs => $1) LIMIT $2)`, olderThan, diffSweepBatch)
	if err != nil {
		return n, fmt.Errorf("store: sweep diffs: %w", err)
	}
	return n, nil
}
