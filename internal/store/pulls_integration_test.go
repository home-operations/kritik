//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPausePullRequest checks that automatic reviews pause on request or
// once their count reaches the repository's maximum, that a request to
// review still finds the pull request paused until it is resumed, and
// that resuming starts the count over.
func TestPausePullRequest(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, soloAccount("paused"))); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	account := accountID(t, s, "paused")
	var repoID, pull string
	if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE account_id = $1 AND name = 'paused/one'`, account).Scan(&repoID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, head_sha)
			VALUES ($1, $2, 7, 'h') RETURNING id`, account, repoID).Scan(&pull)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	paused := func() bool {
		t.Helper()
		var p bool
		if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
			var err error
			p, err = PullRequestPaused(ctx, tx, repoID, 7)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	count := func(maxAuto int) bool {
		t.Helper()
		var now bool
		if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
			var err error
			now, err = CountAutoReview(ctx, tx, pull, maxAuto)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return now
	}
	set := func(p bool) {
		t.Helper()
		if err := s.WithAccount(ctx, account, func(tx pgx.Tx) error { return PausePullRequest(ctx, tx, pull, p) }); err != nil {
			t.Fatal(err)
		}
	}
	if paused() {
		t.Fatal("a new pull request is not paused")
	}
	if count(0) || paused() {
		t.Fatal("without a maximum, a review never pauses")
	}
	if count(3) || paused() {
		t.Fatal("the second of three reviews does not pause")
	}
	if !count(3) || !paused() {
		t.Fatal("the third of three reviews pauses")
	}
	if count(3) || !paused() {
		t.Fatal("a review of a paused pull request reports no new pause and leaves it paused")
	}
	set(false)
	if paused() || count(3) || paused() {
		t.Fatal("resuming starts the count over")
	}
	set(true)
	if !paused() {
		t.Fatal("a request pauses")
	}
}
