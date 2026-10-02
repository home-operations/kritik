//go:build integration

package worker

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
	"github.com/home-operations/kritika/internal/store/storetest"
)

// TestThreadWorker: resolving one of the bot's finding threads dismisses
// the finding, for a sender with write access only, and unresolving it
// takes the dismissal back.
func TestThreadWorker(t *testing.T) {
	ctx := context.Background()
	appStore := storetest.Open(t)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s3cret")
	file := configfiletest.Load(t, configYAML)
	if err := appStore.ApplyConfig(ctx, file); err != nil {
		t.Fatal(err)
	}
	account, _ := file.Account(configfile.ForgeGitHub, "onedr0p")

	const number = 7
	const root, other int64 = commentBase + 1, commentBase + 2
	finding := review.Finding{Path: "main.go", Line: 3, Severity: review.SeverityNit, Title: "Unchecked error", Explanation: "why"}
	fingerprint := review.Fingerprint(finding)
	var repoID, prID string
	if err := appStore.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
		var err error
		repoID, _, err = store.EnsureRepository(ctx, tx, account.ID(), store.ReachedRepository{FullName: "onedr0p/home-ops", DefaultBranch: "main"})
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO pull_requests (account_id, repository_id, number, head_sha)
			VALUES ($1, $2, $3, 'h1') RETURNING id`, account.ID(), repoID, number).Scan(&prID); err != nil {
			return err
		}
		var reviewID string
		if err := tx.QueryRow(ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, status)
			VALUES ($1, $2, 'h1', 'completed') RETURNING id`, account.ID(), prID).Scan(&reviewID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO findings (account_id, review_id, path, line, severity, title, explanation, fingerprint, forge_comment_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, account.ID(), reviewID, finding.Path, finding.Line, string(finding.Severity),
			finding.Title, finding.Explanation, fingerprint, root)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	lf := &localForge{
		permissions: map[string]forge.Permission{"onedr0p": forge.PermissionAdmin, "outsider": forge.PermissionRead},
		threads: map[int64]forge.Comment{
			root:  {ID: root, Author: "kritika[bot]", Body: review.FindingMarker(fingerprint) + "\n**Unchecked error**", Inline: true},
			other: {ID: other, Author: "onedr0p", Body: "nit: rename this", Inline: true},
		},
	}
	base := Base{Store: appStore, Current: configfile.NewCurrent(file), Forges: &forges{f: lf}, Logger: slog.New(slog.DiscardHandler)}
	w := &Thread{Base: base}
	work := func(commentID int64, resolved bool, sender string) {
		t.Helper()
		err := w.Work(ctx, &river.Job[jobs.ThreadArgs]{Args: jobs.ThreadArgs{
			AccountID: account.ID(), RepositoryID: repoID, Number: number, CommentID: commentID, Resolved: resolved, Sender: sender,
		}})
		if err != nil {
			t.Fatalf("Work(%d, %v, %s): %v", commentID, resolved, sender, err)
		}
	}
	dismissals := func() []store.Dismissal {
		t.Helper()
		var ds []store.Dismissal
		if err := appStore.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			var err error
			ds, err = store.Dismissals(ctx, tx, prID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ds
	}

	work(root, true, "outsider")
	if ds := dismissals(); len(ds) != 0 {
		t.Fatalf("a reader's resolution dismissed %+v", ds)
	}
	work(other, true, "onedr0p")
	if ds := dismissals(); len(ds) != 0 {
		t.Fatalf("resolving a thread that is not a finding's dismissed %+v", ds)
	}
	work(root, true, "onedr0p")
	ds := dismissals()
	if len(ds) != 1 || ds[0].Fingerprint != fingerprint || ds[0].Finding.Title != finding.Title ||
		ds[0].Author != "onedr0p" || ds[0].Reason != "thread resolved by onedr0p" || ds[0].CommentID != root {
		t.Fatalf("Dismissals after a maintainer resolved the thread = %+v", ds)
	}
	work(root, false, "outsider")
	if ds := dismissals(); len(ds) != 1 {
		t.Fatalf("a reader's unresolution restored the finding: %+v", ds)
	}
	work(root, false, "onedr0p")
	if ds := dismissals(); len(ds) != 0 {
		t.Fatalf("Dismissals after the thread was unresolved = %+v", ds)
	}
	// Nothing to take back is not a failure.
	work(root, false, "onedr0p")
}
