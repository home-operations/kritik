package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/review"
)

// approvalForge records the approval calls a publish makes.
type approvalForge struct {
	forge.Client
	approved, dismissed string
	err                 error
}

func (f *approvalForge) Approve(_ context.Context, owner, repo string, number int, headSHA, body string) (bool, error) {
	f.approved = owner + "/" + repo + "#" + string(rune('0'+number)) + "@" + headSHA + ": " + body
	return f.err == nil, f.err
}

func (f *approvalForge) DismissApprovals(_ context.Context, owner, repo string, number int, message string) (int, error) {
	f.dismissed = owner + "/" + repo + "#" + string(rune('0'+number)) + ": " + message
	return 1, f.err
}

func TestApprove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                string
		counts              review.Counts
		err                 error
		approved, dismissed string
	}{
		{name: "nothing found approves", approved: "o/r#7@abcdef1234: kritika: nothing blocking or important found at abcdef1."},
		{name: "nits alone approve", counts: review.Counts{Nit: 3}, approved: "o/r#7@abcdef1234: kritika: nothing blocking or important found at abcdef1."},
		{name: "an important finding withdraws", counts: review.Counts{Important: 1, Nit: 1}, dismissed: "o/r#7: kritika: 0 blocking and 1 important finding(s) at abcdef1."},
		{name: "a blocking finding withdraws", counts: review.Counts{Blocking: 2}, dismissed: "o/r#7: kritika: 2 blocking and 0 important finding(s) at abcdef1."},
		{name: "a forge error is logged, not raised", err: errors.New("forbidden"), approved: "o/r#7@abcdef1234: kritika: nothing blocking or important found at abcdef1."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &approvalForge{err: tt.err}
			p := &publishPhase{client: f, pr: &pullRequest{repository: "o/r", number: 7, headSHA: "abcdef1234"}, logger: slog.New(slog.DiscardHandler)}
			p.approve(t.Context(), tt.counts)
			if f.approved != tt.approved || f.dismissed != tt.dismissed {
				t.Fatalf("approved %q dismissed %q, want %q and %q", f.approved, f.dismissed, tt.approved, tt.dismissed)
			}
		})
	}
}
