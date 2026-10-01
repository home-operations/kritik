package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/review"
)

// Dismissal is a finding a maintainer dismissed on a pull request, by the
// fingerprint it keeps across the pull request's reviews, as the finding
// last read, with the reason they gave.
type Dismissal struct {
	AccountID, PullRequestID string
	Fingerprint              string
	Finding                  review.Finding
	Reason, Author           string
	// CommentID is the comment that dismissed it.
	CommentID int64
}

// RecordDismissal records a dismissal, replacing an earlier one of the
// same finding.
func RecordDismissal(ctx context.Context, tx pgx.Tx, d Dismissal) error {
	f := d.Finding
	if _, err := tx.Exec(ctx, `INSERT INTO dismissals
		(account_id, pull_request_id, fingerprint, path, line, severity, title, explanation, reason, author, comment_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, left($9, 500), $10, $11)
		ON CONFLICT (pull_request_id, fingerprint) DO UPDATE SET reason = excluded.reason, author = excluded.author,
			comment_id = excluded.comment_id, created_at = now()`,
		d.AccountID, d.PullRequestID, d.Fingerprint, f.Path, f.Line, string(f.Severity), f.Title, f.Explanation,
		d.Reason, d.Author, d.CommentID); err != nil {
		return fmt.Errorf("store: record dismissal: %w", err)
	}
	return nil
}

// Dismissals lists the pull request's dismissed findings, oldest first.
func Dismissals(ctx context.Context, tx pgx.Tx, pullRequestID string) ([]Dismissal, error) {
	rows, err := tx.Query(ctx, `SELECT account_id, pull_request_id, fingerprint, path, line, severity, title, explanation, reason, author,
		comment_id FROM dismissals WHERE pull_request_id = $1 ORDER BY created_at, fingerprint`, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list dismissals: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Dismissal, error) {
		var d Dismissal
		var sev string
		err := row.Scan(&d.AccountID, &d.PullRequestID, &d.Fingerprint, &d.Finding.Path, &d.Finding.Line, &sev, &d.Finding.Title,
			&d.Finding.Explanation, &d.Reason, &d.Author, &d.CommentID)
		d.Finding.Severity = review.Severity(sev)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list dismissals: %w", err)
	}
	return out, nil
}

// LatestFinding is the pull request's latest report of the finding with
// fingerprint, by its latest completed review, and whether one exists.
func LatestFinding(ctx context.Context, tx pgx.Tx, pullRequestID, fingerprint string) (review.Finding, bool, error) {
	var f review.Finding
	var sev string
	var cat string
	err := tx.QueryRow(ctx, `SELECT f.path, f.line, f.severity, f.category, f.title, f.explanation FROM findings f
		JOIN reviews v ON v.id = f.review_id
		WHERE v.pull_request_id = $1 AND v.status = 'completed' AND f.fingerprint = $2 ORDER BY v.created_at DESC, v.id DESC LIMIT 1`,
		pullRequestID, fingerprint).Scan(&f.Path, &f.Line, &sev, &cat, &f.Title, &f.Explanation)
	if errors.Is(err, pgx.ErrNoRows) {
		return review.Finding{}, false, nil
	}
	if err != nil {
		return review.Finding{}, false, fmt.Errorf("store: read latest finding: %w", err)
	}
	f.Severity, f.Category = review.Severity(sev), review.Category(cat)
	return f, true, nil
}
