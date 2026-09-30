package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/review"
)

// priorReview is a pull request's last completed review; id is "" when it
// has none.
type priorReview struct {
	id, headSHA string
	findings    []priorFinding
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
	err := tx.QueryRow(ctx, `SELECT id, head_sha FROM reviews
		WHERE pull_request_id = $1 AND status = 'completed' ORDER BY created_at DESC LIMIT 1`, prID).
		Scan(&p.id, &p.headSHA)
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

// reviewFindings drops the bookkeeping from prior findings.
func reviewFindings(prior []priorFinding) []review.Finding {
	out := make([]review.Finding, len(prior))
	for i, f := range prior {
		out[i] = f.Finding
	}
	return out
}

// inlineComment is whether a finding has an inline comment on the forge,
// and its id there, 0 when the forge did not say.
type inlineComment struct {
	posted bool
	id     int64
}

// alreadyInline reports, per finding, the inline comment a prior finding
// with the same fingerprint already has on the forge, in which case it is
// not posted again and its thread carries on.
func alreadyInline(findings []review.Finding, prior []priorFinding) []inlineComment {
	posted := map[string]inlineComment{}
	for _, p := range prior {
		if p.postedInline {
			posted[review.Fingerprint(p.Finding)] = inlineComment{posted: true, id: p.commentID}
		}
	}
	out := make([]inlineComment, len(findings))
	for i, f := range findings {
		out[i] = posted[review.Fingerprint(f)]
	}
	return out
}
