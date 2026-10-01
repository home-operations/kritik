//go:build integration

package worker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

// modelCalls lists model calls as accountID sees them, through read.
func modelCalls(
	ctx context.Context, t *testing.T, st *store.Store, accountID string, read func(pgx.Tx) ([]transcript.StoredRow, error),
) []transcript.StoredRow {
	t.Helper()
	var rows []transcript.StoredRow
	if err := st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		rows, err = read(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// checkReviewTranscript checks that the gateway recorded a review's one
// agent step: the system prompt, the prompt, the agent's tools and
// submit_review's input.
func checkReviewTranscript(ctx context.Context, t *testing.T, st *store.Store, accountID, head string, fc *fakeCompleter) {
	t.Helper()
	var reviewID string
	if err := st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM reviews WHERE head_sha = $1 AND status = 'completed'`, head).Scan(&reviewID)
	}); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	system, user := fc.systems[len(fc.systems)-1], fc.users[len(fc.users)-1]
	fc.mu.Unlock()
	conv := transcript.Rebuild(modelCalls(ctx, t, st, accountID, func(tx pgx.Tx) ([]transcript.StoredRow, error) {
		return store.ReviewModelCalls(ctx, tx, reviewID)
	}))
	tools := make([]string, len(conv.Tools))
	for i, tool := range conv.Tools {
		tools[i] = tool.Name
	}
	// The repository is indexed by then, so the agent is offered search_code.
	if len(conv.Turns) != 1 || conv.System != system || !slices.Equal(tools, []string{"read_file", "grep", "list_files", "search_code", "submit_review"}) {
		t.Fatalf("conversation = %+v", conv)
	}
	turn := conv.Turns[0]
	if turn.Kind != store.ModelCallAgentStep || len(turn.Messages) != 1 || turn.Messages[0].Text != user ||
		len(turn.Response.ToolCalls) != 1 || !strings.Contains(string(turn.Response.ToolCalls[0].Input), "first line") ||
		turn.Usage.Input != 10 || turn.Usage.Output != 5 || turn.CostUSD != 0.001 || turn.Upstream != "test" || turn.RunnerRunID == "" {
		t.Fatalf("turn = %+v", turn)
	}
}

// checkFollowUpTranscript checks that answering commentID recorded one
// followup call against the review it followed.
func checkFollowUpTranscript(ctx context.Context, t *testing.T, st *store.Store, accountID string, commentID int64, fc *fakeCompleter) {
	t.Helper()
	fc.mu.Lock()
	user := fc.users[len(fc.users)-1]
	fc.mu.Unlock()
	rows := modelCalls(ctx, t, st, accountID, func(tx pgx.Tx) ([]transcript.StoredRow, error) {
		var prID string
		if err := tx.QueryRow(ctx, `SELECT pull_request_id FROM followups WHERE comment_id = $1`, commentID).Scan(&prID); err != nil {
			return nil, err
		}
		return store.FollowupModelCalls(ctx, tx, prID, commentID)
	})
	if len(rows) != 1 || rows[0].Kind != store.ModelCallFollowUp || rows[0].ReviewID == "" || rows[0].Messages[0].Text != user ||
		!strings.Contains(string(rows[0].Response.ToolCalls[0].Input), "Because b is new.") {
		t.Fatalf("follow-up model calls = %+v", rows)
	}
}
