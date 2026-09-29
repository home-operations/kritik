package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/review"
)

// FindingStatus is whether a finding was addressed: a later completed
// review of its pull request, at another head, no longer reported it.
// An incremental review re-checks each finding of the last one and
// reports it again only while it is still present.
type FindingStatus string

// Finding statuses.
const (
	FindingOpen      FindingStatus = "open"
	FindingAddressed FindingStatus = "addressed"
)

// Valid reports whether s is a finding status.
func (s FindingStatus) Valid() bool { return s == FindingOpen || s == FindingAddressed }

// AccountFinding is one finding of a pull request, however many of its
// reviews reported it, as the latest of them did.
type AccountFinding struct {
	FindingRow
	ReviewID    string
	PullRequest PullRef
	Status      FindingStatus
	// FirstSeenAt and LastSeenAt are when the first and the latest
	// reviews that reported it ran.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// PullRef names a pull request.
type PullRef struct {
	Repository string
	Number     int
	Title      string
	URL        string
}

// FindingFilter narrows ListAccountFindings. Zero fields match everything;
// Query matches a title, explanation, path or pull request title
// substring, or a pull request number.
type FindingFilter struct {
	RepositoryID string
	Severity     review.Severity
	Status       FindingStatus
	Query        string
}

// findingIssues are the common table expressions over every finding of
// the account's completed reviews: seen is each report of one, and latest
// one row per pull request and fingerprint, as its latest review reported
// it, with when it was first reported and whether a later completed review
// at another head dropped it. A finding stored without a fingerprint is
// its own.
const findingIssues = `seen AS (
		SELECT f.id, f.path, f.line, f.end_line, f.severity, f.title, f.explanation, f.suggested_fix, f.replacement,
			f.agent_prompt, f.fingerprint, f.posted_inline, f.forge_comment_id, f.created_at, f.reactions_up, f.reactions_down,
			v.id AS review_id, v.pull_request_id, v.head_sha, v.created_at AS seen_at,
			row_number() OVER newest AS nth, min(v.created_at) OVER issue AS first_at
		FROM findings f JOIN reviews v ON v.id = f.review_id
		WHERE v.status = 'completed'
		WINDOW issue AS (PARTITION BY v.pull_request_id, coalesce(nullif(f.fingerprint, ''), f.id::text)),
			newest AS (issue ORDER BY v.created_at DESC, v.id DESC)),
	latest AS (
		SELECT s.*, EXISTS (SELECT 1 FROM reviews n WHERE n.pull_request_id = s.pull_request_id AND n.status = 'completed'
			AND n.created_at > s.seen_at AND n.head_sha <> s.head_sha) AS addressed
		FROM seen s WHERE s.nth = 1)`

const accountFindings = `WITH ` + findingIssues + `
	SELECT l.id, l.path, l.line, l.end_line, l.severity, l.title, l.explanation, l.suggested_fix, l.replacement,
		l.agent_prompt, l.fingerprint, l.posted_inline, l.forge_comment_id, l.created_at, l.reactions_up, l.reactions_down,
		l.review_id, r.name, p.number, p.title, p.url, l.addressed, l.first_at, l.seen_at
	FROM latest l JOIN pull_requests p ON p.id = l.pull_request_id JOIN repositories r ON r.id = p.repository_id`

// ListAccountFindings returns a page of the account's findings, most
// recently reported first.
func ListAccountFindings(ctx context.Context, tx pgx.Tx, f FindingFilter, p Page) ([]AccountFinding, *Cursor, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	if (f.Severity != "" && !f.Severity.Valid()) || (f.Status != "" && !f.Status.Valid()) {
		return nil, nil, ErrFilter
	}
	number := -1
	if n, err := strconv.Atoi(strings.TrimPrefix(f.Query, "#")); err == nil {
		number = n
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query) + "%"
	rows, err := tx.Query(ctx, accountFindings+`
		WHERE ($1::uuid IS NULL OR p.repository_id = $1)
			AND ($2 = '' OR l.severity = $2)
			AND ($3 = '' OR l.addressed = ($3 = 'addressed'))
			AND ($4 = '' OR l.title ILIKE $5 OR l.explanation ILIKE $5 OR l.path ILIKE $5 OR p.title ILIKE $5 OR p.number = $6)
			AND ($7 OR (l.seen_at, l.id) < ($8, $9::uuid))
		ORDER BY l.seen_at DESC, l.id DESC LIMIT $10`,
		uuidParam(f.RepositoryID), string(f.Severity), string(f.Status), f.Query, like, number,
		p.After.First(), p.After.T, p.afterID(), p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list account findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccountFinding, error) {
		var a AccountFinding
		var sev string
		var addressed bool
		err := row.Scan(&a.ID, &a.Path, &a.Line, &a.EndLine, &sev, &a.Title, &a.Explanation, &a.SuggestedFix, &a.Replacement,
			&a.AgentPrompt, &a.Fingerprint, &a.PostedInline, &a.ForgeCommentID, &a.CreatedAt, &a.ReactionsUp, &a.ReactionsDown,
			&a.ReviewID, &a.PullRequest.Repository, &a.PullRequest.Number, &a.PullRequest.Title, &a.PullRequest.URL,
			&addressed, &a.FirstSeenAt, &a.LastSeenAt)
		a.Severity, a.Status = review.Severity(sev), FindingOpen
		if addressed {
			a.Status = FindingAddressed
		}
		return a, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: list account findings: %w", err)
	}
	items, next := paged(out, p.Limit, func(a AccountFinding) Cursor { return Cursor{T: a.LastSeenAt, ID: a.ID} })
	return items, next, nil
}
