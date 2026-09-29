//go:build integration

package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestReadAnalytics checks the window totals, a series with every bucket
// present, and the repositories' activity, over reviews and findings on
// either side of the window.
func TestReadAnalytics(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, soloAccount("analytics"))); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	account := accountID(t, s, "analytics")
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 3)
	day := func(d int, h int) time.Time { return from.AddDate(0, 0, d).Add(time.Duration(h) * time.Hour) }

	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		var repoID string
		if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE account_id = $1 AND name = 'analytics/one'`, account).Scan(&repoID); err != nil {
			return err
		}
		pull := func(number int) string {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, head_sha)
				VALUES ($1, $2, $3, 'h') RETURNING id`, account, repoID, number).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		review := func(pr, head, status string, at time.Time, took time.Duration, findings ...[2]string) {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status, created_at, finished_at)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, account, pr, head, status, at, at.Add(took)).Scan(&id); err != nil {
				t.Fatal(err)
			}
			for _, f := range findings {
				if _, err := tx.Exec(ctx, `INSERT INTO findings (account_id, review_id, path, line, severity, title, explanation, fingerprint)
					VALUES ($1, $2, 'a.go', 1, $3, $4, '', $4)`, account, id, f[0], f[1]); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO usage (account_id, repository_id, review_id, role, model, cost_usd, created_at)
				VALUES ($1, $2, $3, 'review', 'acme/large', 0.5, $4)`, account, repoID, id, at); err != nil {
				t.Fatal(err)
			}
		}
		a, b := pull(1), pull(2)
		// Before the window: its finding was first reported then, so it counts there.
		review(a, "h0", "completed", day(-1, 12), time.Minute, [2]string{"blocking", "old"})
		review(a, "h1", "completed", day(0, 12), 2*time.Minute, [2]string{"blocking", "old"}, [2]string{"nit", "naming"})
		review(a, "h2", "completed", day(2, 12), 4*time.Minute, [2]string{"important", "race"})
		review(b, "h1", "failed", day(1, 12), time.Minute)
		review(b, "h1", "completed", day(1, 13), 3*time.Minute, [2]string{"nit", "typo"})
		// After the window.
		review(b, "h2", "completed", day(3, 1), time.Minute)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var totals, before AnalyticsTotals
	var series []AnalyticsPoint
	var repos []RepoActivity
	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		var err error
		if totals, err = ReadAnalyticsTotals(ctx, tx, from, to); err != nil {
			return err
		}
		if before, err = ReadAnalyticsTotals(ctx, tx, from.AddDate(0, 0, -3), from); err != nil {
			return err
		}
		if series, err = ReadAnalyticsSeries(ctx, tx, AnalyticsByDay, from, to); err != nil {
			return err
		}
		repos, err = ReadRepoActivity(ctx, tx, from, to, 5)
		return err
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	// naming was dropped by the review at h2, and typo by the one after the
	// window; race is still open. old counts before the window.
	median := int64(3 * time.Minute / time.Millisecond)
	want := AnalyticsTotals{
		PullRequests: 2, Reviews: 3, Failed: 1, Findings: SeverityCounts{Important: 1, Nit: 2}, Addressed: 2, CostUSD: 2,
		MedianReviewMs: &median,
	}
	if !reflect.DeepEqual(totals, want) {
		t.Errorf("totals = %+v (median %v), want %+v (median %v)", totals, deref(totals.MedianReviewMs), want, median)
	}
	if before.Reviews != 1 || before.Findings.Blocking != 1 || before.Addressed != 1 {
		t.Errorf("before = %+v, want the one review and its blocking finding, addressed", before)
	}
	wantSeries := []AnalyticsPoint{
		{Key: "2026-09-01", Reviews: 1, Findings: SeverityCounts{Nit: 1}, CostUSD: 0.5},
		{Key: "2026-09-02", Reviews: 1, Findings: SeverityCounts{Nit: 1}, CostUSD: 1},
		{Key: "2026-09-03", Reviews: 1, Findings: SeverityCounts{Important: 1}, CostUSD: 0.5},
	}
	if !reflect.DeepEqual(series, wantSeries) {
		t.Errorf("series = %+v, want %+v", series, wantSeries)
	}
	wantRepos := []RepoActivity{{Repository: "analytics/one", Reviews: 3, Findings: SeverityCounts{Important: 1, Nit: 2}, Addressed: 2}}
	if !reflect.DeepEqual(repos, wantRepos) {
		t.Errorf("repos = %+v, want %+v", repos, wantRepos)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
