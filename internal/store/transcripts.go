package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/transcript"
)

// ModelCallKind is what made a model call; see transcript.Kind.
type ModelCallKind = transcript.Kind

// Model call kinds, as model_calls.kind spells them.
const (
	ModelCallAgentStep = transcript.KindAgentStep
	ModelCallReview    = transcript.KindReview
	ModelCallFallback  = transcript.KindFallback
	ModelCallFollowUp  = transcript.KindFollowUp
)

// ModelCall is one row of model_calls. ReviewID, RunnerRunID and
// FollowupCommentID are left empty (zero) when the call has none. Row's
// State is what the next agent step of RunnerRunID is a delta against.
type ModelCall struct {
	TenantID          string
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

// InsertModelCall records c in tx, which must be scoped to c's tenant.
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
		(tenant_id, review_id, runner_run_id, followup_comment_id, kind, step, model, upstream, system, tools,
		 messages_from, messages, response, stop_reason, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
		 cost_usd, duration_ms, error, truncated, messages_end, messages_sha, system_sha, tools_sha, run_bytes)
		VALUES ($1, nullif($2, '')::uuid, nullif($3, '')::uuid, nullif($4::bigint, 0), $5, $6, $7, $8, $9, $10::jsonb,
		 $11, $12::jsonb, $13::jsonb, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)`,
		c.TenantID, c.ReviewID, c.RunnerRunID, c.FollowupCommentID, string(c.Kind), c.Step, c.Model, c.Upstream, c.Row.System, tools,
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kritik-transcript:' || $1, 0))`, runnerRunID); err != nil {
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

// ModelCallFilter selects the model calls of one review, one runner run,
// or one follow-up comment; exactly one field is set.
type ModelCallFilter struct {
	ReviewID          string
	RunnerRunID       string
	FollowupCommentID int64
}

// ModelCalls returns the model calls f selects in the order they were
// recorded, as tx's tenant may see them.
func ModelCalls(ctx context.Context, tx pgx.Tx, f ModelCallFilter) ([]transcript.StoredRow, error) {
	var where string
	var arg any
	set := 0
	if f.ReviewID != "" {
		where, arg, set = "review_id = $1::uuid", f.ReviewID, set+1
	}
	if f.RunnerRunID != "" {
		where, arg, set = "runner_run_id = $1::uuid", f.RunnerRunID, set+1
	}
	if f.FollowupCommentID != 0 {
		where, arg, set = "followup_comment_id = $1", f.FollowupCommentID, set+1
	}
	if set != 1 {
		return nil, errors.New("store: a model call filter sets exactly one of review, runner run and follow-up comment")
	}
	return modelCallsWhere(ctx, tx, where, arg)
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

// SweepModelCalls deletes every tenant's model calls recorded more than
// olderThan ago and returns how many it deleted. It runs on the owner
// connection, which row-level security does not restrict. Leader only.
func (s *Store) SweepModelCalls(ctx context.Context, olderThan time.Duration) (int64, error) {
	if s.owner == nil {
		return 0, errors.New("store: SweepModelCalls needs the owner connection")
	}
	if olderThan <= 0 {
		return 0, fmt.Errorf("store: model call retention %s is not positive", olderThan)
	}
	tag, err := s.owner.Exec(ctx, `DELETE FROM model_calls WHERE created_at < now() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: sweep model calls: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SweepSessions deletes the dashboard sessions and in-flight logins that
// expired by now and returns how many it deleted.
func (s *Store) SweepSessions(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.app, func(tx pgx.Tx) error {
		for _, table := range []string{"sessions", "login_states"} {
			tag, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE expires_at <= $1`, now)
			if err != nil {
				return err
			}
			n += tag.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: sweep sessions: %w", err)
	}
	return n, nil
}
