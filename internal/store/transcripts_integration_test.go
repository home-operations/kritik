//go:build integration

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/transcript"
)

func TestModelCalls(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := tenantID(t, s, "alpha"), tenantID(t, s, "beta")
	reviewID := insertReview(t, ctx, s, alpha)
	// Other tests count every model call a tenant has.
	t.Cleanup(func() { deleteModelCalls(t, s, `review_id = $1 OR followup_comment_id = 4242`, reviewID) })
	var runID string
	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, kind, review_id) VALUES ($1, 'review', $2) RETURNING id`,
			alpha, reviewID).Scan(&runID)
	}); err != nil {
		t.Fatal(err)
	}

	tools := []model.ToolDef{{Name: "grep", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}}
	msgs := make([]model.Message, 0, 3)
	msgs = append(msgs, model.Message{Role: model.RoleUser, Text: "review"})
	record := func(req model.StepRequest) {
		t.Helper()
		if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
			prev, step, err := AgentState(ctx, tx, runID)
			if err != nil {
				return err
			}
			r := transcript.Delta(prev, req, nil)
			r.Response = transcript.Response{Text: "ok", Stop: model.StopToolUse}
			return InsertModelCall(ctx, tx, ModelCall{TenantID: alpha, ReviewID: reviewID, RunnerRunID: runID, Kind: ModelCallAgentStep,
				Step: step, Model: "m", Row: r.Encode(), Usage: model.Usage{Input: 10, Output: 2}, CostUSD: 0.25, Duration: 1500 * time.Millisecond})
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(model.StepRequest{System: "sys", Messages: msgs, Tools: tools})
	msgs = append(msgs, model.Message{Role: model.RoleAssistant, Text: "looking"}, model.Message{Role: model.RoleUser, Text: "more"})
	record(model.StepRequest{System: "sys", Messages: msgs, Tools: tools})

	list := func(tenant string, f ModelCallFilter) []transcript.StoredRow {
		t.Helper()
		var rows []transcript.StoredRow
		if err := s.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
			var err error
			rows, err = ModelCalls(ctx, tx, f)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := list(alpha, ModelCallFilter{RunnerRunID: runID})
	if len(rows) != 2 || rows[0].Step != 0 || rows[1].Step != 1 || rows[1].MessagesFrom != 1 || len(rows[1].Messages) != 2 ||
		rows[0].System == nil || rows[1].System != nil || rows[1].Tools != nil || rows[0].Duration != 1500*time.Millisecond ||
		rows[0].CostUSD != 0.25 || rows[0].Usage.Input != 10 || rows[0].ReviewID != reviewID {
		t.Fatalf("rows = %+v", rows)
	}
	conv := transcript.Rebuild(list(alpha, ModelCallFilter{ReviewID: reviewID}))
	if conv.System != "sys" || len(conv.Tools) != 1 || len(conv.Turns) != 2 || conv.Turns[1].Messages[1].Text != "more" {
		t.Fatalf("conversation = %+v", conv)
	}
	if n := len(list(beta, ModelCallFilter{ReviewID: reviewID})); n != 0 {
		t.Fatalf("beta sees %d of alpha's model calls", n)
	}

	// A follow-up row is found by its comment, and has no run.
	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		r := transcript.Delta(transcript.State{}, model.StepRequest{System: "f", Messages: msgs[:1]}, nil)
		return InsertModelCall(ctx, tx, ModelCall{TenantID: alpha, FollowupCommentID: 4242, Kind: ModelCallFollowUp, Row: r.Encode()})
	}); err != nil {
		t.Fatal(err)
	}
	if rows := list(alpha, ModelCallFilter{FollowupCommentID: 4242}); len(rows) != 1 || rows[0].RunnerRunID != "" || rows[0].ReviewID != "" {
		t.Fatalf("follow-up rows = %+v", rows)
	}

	checkModelCallRefusals(t, s, alpha, beta, reviewID, runID)
}

// checkModelCallRefusals checks what ModelCalls and InsertModelCall refuse.
func checkModelCallRefusals(t *testing.T, s *Store, alpha, beta, reviewID, runID string) {
	ctx := context.Background()
	if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
		if _, err := ModelCalls(ctx, tx, ModelCallFilter{ReviewID: reviewID, RunnerRunID: runID}); err == nil {
			t.Error("a filter with two fields was accepted")
		}
		if err := InsertModelCall(ctx, tx, ModelCall{TenantID: alpha, Kind: "other"}); err == nil {
			t.Error("an unknown kind was inserted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Another tenant cannot write into alpha's transcript either.
	if err := s.WithTenant(ctx, beta, func(tx pgx.Tx) error {
		return InsertModelCall(ctx, tx, ModelCall{TenantID: alpha, Kind: ModelCallReview, Row: transcript.Delta(transcript.State{},
			model.StepRequest{}, nil).Encode()})
	}); err == nil {
		t.Fatal("beta inserted a model call for alpha")
	}
}

func deleteModelCalls(t *testing.T, s *Store, where string, args ...any) {
	t.Helper()
	if _, err := s.owner.Exec(context.Background(), `DELETE FROM model_calls WHERE `+where, args...); err != nil {
		t.Error(err)
	}
}

func TestSweepModelCalls(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := tenantID(t, s, "alpha")
	insert := func() string {
		t.Helper()
		var id string
		if err := s.WithTenant(ctx, alpha, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO model_calls (tenant_id, kind) VALUES ($1, 'review') RETURNING id`, alpha).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, fresh := insert(), insert()
	t.Cleanup(func() { deleteModelCalls(t, s, `id IN ($1, $2)`, old, fresh) })
	if _, err := s.owner.Exec(ctx, `UPDATE model_calls SET created_at = now() - interval '40 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepModelCalls(ctx, 0); err == nil {
		t.Fatal("a zero retention was accepted")
	}
	n, err := s.SweepModelCalls(ctx, 30*24*time.Hour)
	if err != nil || n < 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	var left []string
	if err := s.owner.QueryRow(ctx, `SELECT array_agg(id::text) FROM model_calls WHERE id IN ($1, $2)`, old, fresh).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != fresh {
		t.Fatalf("left = %v, want only %s", left, fresh)
	}
}

func TestSweepSessions(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	var account string
	if err := s.app.QueryRow(ctx, `INSERT INTO accounts (display_name) VALUES ('sweep') RETURNING id`).Scan(&account); err != nil {
		t.Fatal(err)
	}
	for i, expires := range []time.Time{now.Add(-time.Minute), now.Add(time.Hour)} {
		key := []byte{byte(i), 's', 'w', 'e', 'e', 'p', byte(now.UnixNano())}
		if _, err := s.app.Exec(ctx, `INSERT INTO sessions (token_hash, account_id, provider, expires_at) VALUES ($1, $2, 'github', $3)`,
			key, account, expires); err != nil {
			t.Fatal(err)
		}
		if _, err := s.app.Exec(ctx, `INSERT INTO login_states (state_hash, provider, nonce, pkce_verifier, expires_at, browser_hash)
			VALUES ($1, 'github', 'n', 'v', $2, $1)`, key, expires); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SweepSessions(ctx, now)
	if err != nil || n < 2 {
		t.Fatalf("swept %d, %v", n, err)
	}
	var sessions, states int
	if err := s.app.QueryRow(ctx, `SELECT (SELECT count(*) FROM sessions WHERE account_id = $1),
		(SELECT count(*) FROM login_states WHERE expires_at > $2)`, account, now).Scan(&sessions, &states); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || states < 1 {
		t.Fatalf("sessions left %d, live login states %d", sessions, states)
	}
	var expired int
	if err := s.app.QueryRow(ctx, `SELECT count(*) FROM login_states WHERE expires_at <= $1`, now).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired login states left %d, %v", expired, err)
	}
}
