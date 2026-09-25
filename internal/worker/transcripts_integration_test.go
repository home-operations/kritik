//go:build integration

package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

func modelCalls(ctx context.Context, t *testing.T, st *store.Store, tenantID string, f store.ModelCallFilter) []transcript.StoredRow {
	t.Helper()
	var rows []transcript.StoredRow
	if err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		rows, err = store.ModelCalls(ctx, tx, f)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// checkSingleShotTranscript checks that a single-mode review recorded its
// one call: the system prompt, the prompt and the forced tool's input.
func checkSingleShotTranscript(ctx context.Context, t *testing.T, st *store.Store, tenantID, head string, fc *fakeCompleter) {
	t.Helper()
	var reviewID string
	if err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM reviews WHERE head_sha = $1 AND status = 'completed'`, head).Scan(&reviewID)
	}); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	system, user := fc.systems[len(fc.systems)-1], fc.users[len(fc.users)-1]
	fc.mu.Unlock()
	conv := transcript.Rebuild(modelCalls(ctx, t, st, tenantID, store.ModelCallFilter{ReviewID: reviewID}))
	if len(conv.Turns) != 1 || conv.System != system || len(conv.Tools) != 1 || conv.Tools[0].Name != "findings" {
		t.Fatalf("conversation = %+v", conv)
	}
	turn := conv.Turns[0]
	if turn.Kind != store.ModelCallReview || len(turn.Messages) != 1 || turn.Messages[0].Text != user ||
		len(turn.Response.ToolCalls) != 1 || !strings.Contains(string(turn.Response.ToolCalls[0].Input), "first line") ||
		turn.Usage.Input != 10 || turn.Usage.Output != 5 || turn.CostUSD != 0.001 || turn.Upstream != "test" || turn.RunnerRunID != "" {
		t.Fatalf("turn = %+v", turn)
	}
}

// checkFollowUpTranscript checks that answering commentID recorded one
// followup call against the review it followed.
func checkFollowUpTranscript(ctx context.Context, t *testing.T, st *store.Store, tenantID string, commentID int64, fc *fakeCompleter) {
	t.Helper()
	fc.mu.Lock()
	user := fc.users[len(fc.users)-1]
	fc.mu.Unlock()
	rows := modelCalls(ctx, t, st, tenantID, store.ModelCallFilter{FollowupCommentID: commentID})
	if len(rows) != 1 || rows[0].Kind != store.ModelCallFollowUp || rows[0].ReviewID == "" || rows[0].Messages[0].Text != user ||
		!strings.Contains(string(rows[0].Response.ToolCalls[0].Input), "Because b is new.") {
		t.Fatalf("follow-up model calls = %+v", rows)
	}
}
