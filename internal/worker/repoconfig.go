package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

// Effective is a repository's settings once its .kritika.yaml is applied.
// Settings.Review names the files the runner reads from the merge base and
// keeps in the review's context pack.
type Effective struct {
	repoconfig.Merged
	// Found is whether the repository has a .kritika.yaml.
	Found bool
}

// readRepoConfig reads .kritika.yaml at ref through the forge: nil when the
// repository has none, and nil with a note when it is too large to use.
func readRepoConfig(ctx context.Context, client forge.Client, owner, repo, ref string) ([]byte, []string, error) {
	doc, err := client.FileAt(ctx, owner, repo, ref, repoconfig.FileName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, nil
	case errors.Is(err, forge.ErrFileTooLarge) || err == nil && len(doc) > repoconfig.MaxFileBytes:
		return nil, []string{repoconfig.TooLarge(repoconfig.FileName)}, nil
	case err != nil:
		return nil, nil, err
	}
	return doc, nil, nil
}

// effective applies doc, the merge-base .kritika.yaml or nil when there is
// none, onto the admin's settings (see repoconfig.Merge). The notes say
// which of the file's values were dropped, or why the whole file was
// ignored.
func effective(settings configfile.Settings, doc []byte) (Effective, []string) {
	m, err := repoconfig.Merge(doc, settings)
	notes := m.Dropped
	if err != nil {
		notes = append(notes, fmt.Sprintf("%s was ignored: %v", repoconfig.FileName, err))
	}
	return Effective{Merged: m, Found: doc != nil}, notes
}

// repoFiles are the paths the runner reads from the merge base: the files
// the settings name, and .kritika.yaml itself, which the review's context
// pack keeps a copy of.
func (e *Effective) repoFiles() []string {
	paths := e.Review.Referenced()
	if e.Found && !slices.Contains(paths, repoconfig.FileName) {
		paths = append(paths, repoconfig.FileName)
	}
	return paths
}

// templates are the repository's comment templates, read out of files,
// what the runner read from the merge base; a named file it could not
// read leaves the built-in template in use, as the pack's notes say.
func (e *Effective) templates(files repoconfig.Files) review.Templates {
	return review.Templates{Summary: files[e.Review.Templates.Summary], Inline: files[e.Review.Templates.Inline]}
}

// settleLeft is how much longer a review job started by trigger, enqueued
// at created, waits before it runs: a new head waits until settle has
// passed since it arrived, so a burst of pushes is reviewed once, at its
// last head.
func settleLeft(trigger string, settle time.Duration, created, now time.Time) time.Duration {
	if !jobs.Settles(trigger) {
		return 0
	}
	return created.Add(settle).Sub(now)
}

// skipByRepo ends a review the merge-base .kritika.yaml disables or filters
// out before a runner is spent on it, with a success status saying why. It
// reports whether it ended the review, with the error of recording that.
func (w *Review) skipByRepo(ctx context.Context, e earlyEnd, eff *Effective, client forge.Client, owner, repo string) (bool, error) {
	var vars map[string]any
	if err := w.Store.WithAccount(ctx, e.args.AccountID, func(tx pgx.Tx) error {
		var err error
		vars, err = filterVars(ctx, tx, e.pr.id, e.args.Trigger)
		return err
	}); err != nil {
		return true, err
	}
	// With no changed paths yet, only enabled and the filter can skip.
	reason, err := eff.Check(vars, nil)
	if err != nil {
		e.logger.Warn("repository filter failed to evaluate", "error", err)
	}
	if reason == "" {
		return false, nil
	}
	e.logger.Info("review skipped before its runner", "reason", reason)
	e.skip = reason
	if err := w.end(ctx, e, store.ReviewSkipped, ""); err != nil {
		return true, err
	}
	desc := "kritika: skipped (" + reason.Description() + ")"
	if err := client.SetStatus(ctx, owner, repo, e.args.HeadSHA, forge.StatusSuccess, desc); err != nil {
		e.logger.Warn("commit status not set", "error", err)
	}
	return true, nil
}

// filterVars rebuilds the filter's pr variable for a review started by
// trigger from the stored pull request row, the same keys
// webhook.PullRequest.FilterVars gives ingest.
func filterVars(ctx context.Context, tx pgx.Tx, prID, trigger string) (map[string]any, error) {
	pr, err := loadFilterPR(ctx, tx, prID)
	if err != nil {
		return nil, err
	}
	pr.Event = trigger
	return pr.Vars()
}

// loadFilterPR reads what the repository filter sees of a pull request.
func loadFilterPR(ctx context.Context, tx pgx.Tx, prID string) (repoconfig.PullRequest, error) {
	var (
		pr       repoconfig.PullRequest
		openedAt *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT number, title, author, state, merged, draft, fork, head_ref, head_sha, base_ref, url, body,
		opened_at, labels FROM pull_requests WHERE id = $1`, prID).
		Scan(&pr.Number, &pr.Title, &pr.Author, &pr.State, &pr.Merged, &pr.Draft, &pr.Fork, &pr.HeadRef, &pr.HeadSHA, &pr.BaseRef,
			&pr.URL, &pr.Body, &openedAt, &pr.Labels)
	if err != nil {
		return repoconfig.PullRequest{}, fmt.Errorf("worker: read pull request for the filter: %w", err)
	}
	if openedAt != nil {
		pr.CreatedAt = *openedAt
	}
	return pr, nil
}
