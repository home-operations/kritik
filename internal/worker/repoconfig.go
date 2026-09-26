package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

// Effective is a repository's settings once its .kritik.yaml is applied.
// Settings.Review names the files the runner reads; Instructions and
// Templates hold their contents once it has.
type Effective struct {
	repoconfig.Merged
	// Found is whether the repository has a .kritik.yaml.
	Found        bool
	Instructions []string
	Templates    review.Templates
}

// readRepoConfig reads .kritik.yaml at ref through the forge: nil when the
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

// effective applies doc, the merge-base .kritik.yaml or nil when there is
// none, onto the operator's settings (see repoconfig.Merge). The notes say
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
// the settings name, and .kritik.yaml itself, which the review's context
// pack keeps a copy of.
func (e *Effective) repoFiles() []string {
	paths := e.Review.Referenced()
	if e.Found && !slices.Contains(paths, repoconfig.FileName) {
		paths = append(paths, repoconfig.FileName)
	}
	return paths
}

// fill reads the contents of the files e names out of files, what the
// runner read, into Instructions and Templates, leaving out instructions
// scoped to paths none of changed matches. notes lead the returned ones; a
// named file missing from files is noted unless they already say why.
func (e *Effective) fill(files repoconfig.Files, notes, changed []string) []string {
	notes = slices.Clone(notes)
	read := func(p string) string {
		if p == "" {
			return ""
		}
		content, ok := files[p]
		if !ok && !slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, p+": ") }) {
			notes = append(notes, fmt.Sprintf("%s: referenced but not found", p))
		}
		return content
	}
	for _, p := range e.Review.Instructions {
		read(p)
	}
	var truncated bool
	active := repoconfig.Active(e.Review.Instructions, e.Scoped, changed)
	if e.Instructions, truncated = repoconfig.Instructions(files, active); truncated {
		notes = append(notes, "repository instructions truncated to 32 KiB")
	}
	e.Templates = review.Templates{Summary: read(e.Review.Templates.Summary), Inline: read(e.Review.Templates.Inline)}
	return notes
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

// skipByRepo ends a review the merge-base .kritik.yaml disables or filters
// out before a runner is spent on it, with a success status saying why. It
// reports whether it ended the review, with the error of recording that.
func (w *Review) skipByRepo(ctx context.Context, e earlyEnd, eff *Effective, client forge.Client, owner, repo string) (bool, error) {
	var vars map[string]any
	if err := w.Store.WithTenant(ctx, e.args.TenantID, func(tx pgx.Tx) error {
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
	e.logger.Info("review "+statusSkipped+" before its runner", "reason", reason)
	e.skip = reason
	if err := w.end(ctx, e, statusSkipped, ""); err != nil {
		return true, err
	}
	desc := "kritik: skipped (" + reason.Description() + ")"
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
