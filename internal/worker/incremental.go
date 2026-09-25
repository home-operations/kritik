package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/review"
)

// reviewScope is what a review covered; the reviews table's CHECK lists
// the same set.
type reviewScope string

const (
	scopeFull        reviewScope = "full"
	scopeIncremental reviewScope = "incremental"
)

// Valid reports whether s is a known scope.
func (s reviewScope) Valid() bool { return s == scopeFull || s == scopeIncremental }

// decideScope says whether a review can build on the last completed one:
// only when there is one, the runner fetched its head, and fewer than
// maxDeltaFiles files changed since. A full review says why it is one.
func decideScope(hasPrior, priorFetched bool, deltaFiles, maxDeltaFiles int) (reviewScope, string) {
	switch {
	case !hasPrior:
		return scopeFull, "no completed review to build on"
	case !priorFetched:
		return scopeFull, "prior head unreachable"
	case deltaFiles >= maxDeltaFiles:
		return scopeFull, fmt.Sprintf("%d files changed since last review", deltaFiles)
	}
	return scopeIncremental, ""
}

// priorReview is a pull request's last completed review; id is "" when it
// has none.
type priorReview struct {
	id, headSHA string
	findings    []priorFinding
}

type priorFinding struct {
	review.Finding
	postedInline bool
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
	rows, err := tx.Query(ctx, `SELECT path, line, severity, title, explanation, suggested_fix, posted_inline FROM findings
		WHERE review_id = $1 ORDER BY path, line`, p.id)
	if err != nil {
		return priorReview{}, fmt.Errorf("worker: load findings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f priorFinding
		var sev string
		if err := rows.Scan(&f.Path, &f.Line, &sev, &f.Title, &f.Explanation, &f.SuggestedFix, &f.postedInline); err != nil {
			return priorReview{}, fmt.Errorf("worker: scan finding: %w", err)
		}
		f.Severity = review.Severity(sev)
		p.findings = append(p.findings, f)
	}
	if err := rows.Err(); err != nil {
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

// alreadyInline reports, per finding, whether a prior finding with the
// same fingerprint already has an inline comment on the forge, in which
// case it is not posted again.
func alreadyInline(findings []review.Finding, prior []priorFinding) []bool {
	posted := map[string]bool{}
	for _, p := range prior {
		if p.postedInline {
			posted[review.Fingerprint(p.Finding)] = true
		}
	}
	out := make([]bool, len(findings))
	for i, f := range findings {
		out[i] = posted[review.Fingerprint(f)]
	}
	return out
}
