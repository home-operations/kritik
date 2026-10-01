package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

// priorReview is a pull request's last completed review; id is "" when it
// has none. Its findings leave out the ones maintainers dismissed, which
// dismissed holds: their threads are resolved already, and the next
// review is told of them apart.
type priorReview struct {
	id, headSHA, trigger string
	findings             []priorFinding
	dismissed            []store.Dismissal
}

type priorFinding struct {
	review.Finding
	postedInline bool
	// commentID is the inline comment's id on the forge, 0 when unknown.
	commentID int64
}

// lastCompleted loads the pull request's last completed review and its
// findings.
func lastCompleted(ctx context.Context, tx pgx.Tx, prID string) (priorReview, error) {
	var p priorReview
	err := tx.QueryRow(ctx, `SELECT id, head_sha, trigger FROM reviews
		WHERE pull_request_id = $1 AND status = 'completed' ORDER BY created_at DESC LIMIT 1`, prID).
		Scan(&p.id, &p.headSHA, &p.trigger)
	if errors.Is(err, pgx.ErrNoRows) {
		return priorReview{}, nil
	}
	if err != nil {
		return priorReview{}, fmt.Errorf("worker: load last completed review: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT path, line, severity, title, explanation, suggested_fix, posted_inline,
		end_line, replacement, agent_prompt, coalesce(forge_comment_id, 0), rules FROM findings WHERE review_id = $1 ORDER BY path, line`, p.id)
	if err != nil {
		return priorReview{}, fmt.Errorf("worker: load findings: %w", err)
	}
	p.findings, err = pgx.AppendRows(p.findings, rows, func(row pgx.CollectableRow) (priorFinding, error) {
		var f priorFinding
		var sev string
		err := row.Scan(&f.Path, &f.Line, &sev, &f.Title, &f.Explanation, &f.SuggestedFix, &f.postedInline,
			&f.EndLine, &f.Replacement, &f.AgentPrompt, &f.commentID, &f.Rules)
		f.Severity = review.Severity(sev)
		return f, err
	})
	if err != nil {
		return priorReview{}, fmt.Errorf("worker: load findings: %w", err)
	}
	return p, nil
}

// withDismissals gives p the pull request's dismissals and drops the
// findings they name from its findings.
func (p *priorReview) withDismissals(dismissed []store.Dismissal) {
	p.dismissed = dismissed
	gone := make(map[string]bool, len(dismissed))
	for _, d := range dismissed {
		gone[d.Fingerprint] = true
	}
	p.findings = slices.DeleteFunc(p.findings, func(f priorFinding) bool { return gone[review.Fingerprint(f.Finding)] })
}

// dismissedFindings is what the prompt is told of the dismissals.
func dismissedFindings(dismissed []store.Dismissal) []review.DismissedFinding {
	out := make([]review.DismissedFinding, len(dismissed))
	for i, d := range dismissed {
		out[i] = review.DismissedFinding{Finding: d.Finding, Reason: d.Reason}
	}
	return out
}

// dropDismissed leaves out the findings maintainers dismissed on the pull
// request, should the model raise one again, and counts them.
func dropDismissed(findings []review.Finding, dismissed []store.Dismissal) ([]review.Finding, int) {
	if len(dismissed) == 0 {
		return findings, 0
	}
	gone := make(map[string]bool, len(dismissed))
	for _, d := range dismissed {
		gone[d.Fingerprint] = true
	}
	kept := slices.DeleteFunc(slices.Clone(findings), func(f review.Finding) bool { return gone[review.Fingerprint(f)] })
	return kept, len(findings) - len(kept)
}

// reviewFindings drops the bookkeeping from prior findings.
func reviewFindings(prior []priorFinding) []review.Finding {
	out := make([]review.Finding, len(prior))
	for i, f := range prior {
		out[i] = f.Finding
	}
	return out
}

// alreadyInline reports, per finding, the inline comment a prior finding
// with the same fingerprint already has on the forge, in which case it is
// not posted again and its thread carries on.
func alreadyInline(findings []review.Finding, prior []priorFinding) []store.InlinePosted {
	posted := map[string]store.InlinePosted{}
	for _, p := range prior {
		if p.postedInline {
			posted[review.Fingerprint(p.Finding)] = store.InlinePosted{Posted: true, ID: p.commentID}
		}
	}
	out := make([]store.InlinePosted, len(findings))
	for i, f := range findings {
		out[i] = posted[review.Fingerprint(f)]
	}
	return out
}
