//go:build integration

package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/review"
)

// TestListAccountFindings checks that a finding is listed once per pull
// request and fingerprint, as its latest completed review reported it, is
// addressed once a later completed review at another head drops it, and
// is dismissed, not addressed, once a maintainer dismisses it.
func TestListAccountFindings(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, soloAccount("findings"))); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	account := accountID(t, s, "findings")
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	var first, second string
	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		var repoID string
		if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE account_id = $1 AND name = 'findings/one'`, account).Scan(&repoID); err != nil {
			return err
		}
		pull := func(number int, title string) string {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, title, head_sha, url)
				VALUES ($1, $2, $3, $4, 'h', 'https://git.example/pr') RETURNING id`, account, repoID, number, title).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		reviewAt := func(pr, head, status string, at time.Time, findings ...[3]string) {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status, created_at)
				VALUES ($1, $2, $3, $4, $5) RETURNING id`, account, pr, head, status, at).Scan(&id); err != nil {
				t.Fatal(err)
			}
			for _, f := range findings {
				if _, err := tx.Exec(ctx, `INSERT INTO findings (account_id, review_id, path, line, severity, title, explanation, fingerprint)
					VALUES ($1, $2, 'a.go', 3, $3, $4, 'why', $5)`, account, id, f[0], f[1], f[2]); err != nil {
					t.Fatal(err)
				}
			}
		}
		first, second = pull(7, "Add widgets"), pull(8, "More widgets")
		reviewAt(first, "h1", "completed", t0, [3]string{"blocking", "nil deref", "fp-a"}, [3]string{"nit", "naming", "fp-b"})
		// The same head again: a re-run that leaves naming out did not address it.
		reviewAt(first, "h1", "completed", t0.Add(time.Minute), [3]string{"blocking", "nil deref", "fp-a"})
		reviewAt(first, "h2", "completed", t0.Add(2*time.Minute), [3]string{"important", "Nil deref", "fp-a"})
		reviewAt(first, "h3", "running", t0.Add(3*time.Minute))
		reviewAt(second, "h1", "completed", t0.Add(4*time.Minute), [3]string{"nit", "nil deref", "fp-a"})
		// Pull 7's latest report of fp-a cites a rule its earlier ones did not.
		_, err := tx.Exec(ctx, `UPDATE findings SET rules = CASE title WHEN 'Nil deref' THEN '{wrap-errors}'::text[]
			ELSE '{wrap-errors,no-tokens}' END WHERE title IN ('Nil deref', 'naming')`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	list := func(f FindingFilter, p Page) ([]AccountFinding, *Cursor) {
		t.Helper()
		var out []AccountFinding
		var next *Cursor
		if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
			var err error
			out, next, err = ListAccountFindings(ctx, tx, f, p)
			return err
		}); err != nil {
			t.Fatalf("ListAccountFindings(%+v): %v", f, err)
		}
		return out, next
	}
	type row struct {
		number   int
		title    string
		severity review.Severity
		status   FindingStatus
	}
	rows := func(items []AccountFinding) []row {
		out := make([]row, len(items))
		for i, a := range items {
			out[i] = row{a.PullRequest.Number, a.Title, a.Severity, a.Status}
		}
		return out
	}
	check := func(name string, got []AccountFinding, want ...row) {
		t.Helper()
		g := rows(got)
		if len(g) != len(want) {
			t.Fatalf("%s: got %+v, want %+v", name, g, want)
		}
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("%s: got %+v, want %+v", name, g, want)
			}
		}
	}

	all, next := list(FindingFilter{}, Page{Limit: 10})
	check("all", all,
		row{8, "nil deref", review.SeverityNit, FindingOpen},
		row{7, "Nil deref", review.SeverityImportant, FindingOpen},
		row{7, "naming", review.SeverityNit, FindingAddressed},
	)
	if next != nil {
		t.Fatalf("next = %+v, want the end of the list", next)
	}
	if a := all[1]; !a.FirstSeenAt.Equal(t0) || !a.LastSeenAt.Equal(t0.Add(2*time.Minute)) || a.PullRequest.Title != "Add widgets" {
		t.Fatalf("pull 7's finding = %+v, want first seen at t0 and last two minutes later", a)
	}

	addressed, _ := list(FindingFilter{Status: FindingAddressed}, Page{Limit: 10})
	check("addressed", addressed, row{7, "naming", review.SeverityNit, FindingAddressed})
	important, _ := list(FindingFilter{Severity: review.SeverityImportant}, Page{Limit: 10})
	check("important", important, row{7, "Nil deref", review.SeverityImportant, FindingOpen})
	byPull, _ := list(FindingFilter{Query: "More"}, Page{Limit: 10})
	check("pull title", byPull, row{8, "nil deref", review.SeverityNit, FindingOpen})
	byNumber, _ := list(FindingFilter{Query: "#7"}, Page{Limit: 10})
	check("number", byNumber, row{7, "Nil deref", review.SeverityImportant, FindingOpen}, row{7, "naming", review.SeverityNit, FindingAddressed})

	wraps, _ := list(FindingFilter{Rule: "wrap-errors"}, Page{Limit: 10})
	check("rule", wraps, row{7, "Nil deref", review.SeverityImportant, FindingOpen}, row{7, "naming", review.SeverityNit, FindingAddressed})
	if got := wraps[1].Rules; len(got) != 2 || got[0] != "wrap-errors" || got[1] != "no-tokens" {
		t.Fatalf("naming's rules = %q", got)
	}
	var cited []RuleCitation
	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		var err error
		cited, err = RuleCitations(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []RuleCitation{{"findings/one", "no-tokens", 1, 1}, {"findings/one", "wrap-errors", 2, 1}}; !slices.Equal(cited, want) {
		t.Fatalf("RuleCitations = %+v, want %+v", cited, want)
	}

	page1, next := list(FindingFilter{}, Page{Limit: 2})
	if next == nil {
		t.Fatal("a page of two of three has no next cursor")
	}
	page2, _ := list(FindingFilter{}, Page{Limit: 2, After: *next})
	check("paged", append(page1, page2...), rows(all)[0], rows(all)[1], rows(all)[2])
}

// TestDismissedFindings checks that a finding a maintainer dismissed is
// listed dismissed with its reason, never addressed, whatever the reviews
// after it report.
func TestDismissedFindings(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, soloAccount("dismissed"))); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	account := accountID(t, s, "dismissed")
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var pull string
	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		var repoID string
		if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE account_id = $1 AND name = 'dismissed/one'`, account).Scan(&repoID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, title, head_sha, url)
			VALUES ($1, $2, 7, 'Add widgets', 'h', 'https://git.example/pr') RETURNING id`, account, repoID).Scan(&pull); err != nil {
			return err
		}
		for i, head := range []string{"h1", "h2"} {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status, created_at)
				VALUES ($1, $2, $3, 'completed', $4) RETURNING id`, account, pull, head, t0.Add(time.Duration(i)*time.Minute)).Scan(&id); err != nil {
				return err
			}
			// The second review drops naming, which would address it.
			findings := [][2]string{{"nil deref", "fp-a"}, {"naming", "fp-b"}}[:2-i]
			for _, f := range findings {
				if _, err := tx.Exec(ctx, `INSERT INTO findings (account_id, review_id, path, line, severity, title, explanation, fingerprint)
					VALUES ($1, $2, 'a.go', 3, 'nit', $3, 'why', $4)`, account, id, f[0], f[1]); err != nil {
					return err
				}
			}
		}
		finding, found, err := LatestFinding(ctx, tx, pull, "fp-b")
		if err != nil || !found || finding.Title != "naming" || finding.Severity != review.SeverityNit {
			t.Fatalf("LatestFinding = %+v, %v, %v", finding, found, err)
		}
		if _, found, err := LatestFinding(ctx, tx, pull, "fp-none"); err != nil || found {
			t.Fatalf("LatestFinding of an unknown fingerprint = %v, %v", found, err)
		}
		return RecordDismissal(ctx, tx, Dismissal{AccountID: account, PullRequestID: pull, Fingerprint: "fp-b", Finding: finding,
			Reason: "house style", Author: "devin", CommentID: 99})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	statuses := map[FindingStatus][]string{}
	var reason string
	var ds []Dismissal
	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		for _, status := range []FindingStatus{FindingOpen, FindingAddressed, FindingDismissed} {
			items, _, err := ListAccountFindings(ctx, tx, FindingFilter{Status: status}, Page{Limit: 10})
			if err != nil {
				return err
			}
			for _, a := range items {
				statuses[status] = append(statuses[status], a.Title)
				reason += a.DismissReason
			}
		}
		var err error
		ds, err = Dismissals(ctx, tx, pull)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(statuses[FindingOpen], []string{"nil deref"}) || len(statuses[FindingAddressed]) != 0 ||
		!slices.Equal(statuses[FindingDismissed], []string{"naming"}) || reason != "house style" {
		t.Fatalf("statuses = %v with reason %q; want naming dismissed, not addressed", statuses, reason)
	}
	if len(ds) != 1 || ds[0].Fingerprint != "fp-b" || ds[0].Finding.Title != "naming" || ds[0].CommentID != 99 {
		t.Fatalf("Dismissals = %+v", ds)
	}
}

// soloAccount serves one account of its own, with one repository, so a
// test's rows are the only ones its account reads in the shared database.
func soloAccount(name string) string {
	return fmt.Sprintf(`
apps:
  - name: %[1]s-bot
    accounts: [%[1]s]
    clientId: Iv1.test
    privateKey: { env: KRITIKA_TEST_TOKEN }
    webhookSecret: { env: KRITIKA_TEST_TOKEN }
repositories:
  %[1]s/one: {}
`, name)
}
