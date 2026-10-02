package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

// Thread works a review thread someone resolved or unresolved on the
// forge. Resolving one of the bot's finding threads dismisses the finding,
// as "@<bot> dismiss" does; unresolving it takes the dismissal back. Both
// take write access, as the mention does: the forge lets a pull request's
// author resolve the threads on their own pull request, which must not
// silence the reviewer.
type Thread struct {
	river.WorkerDefaults[jobs.ThreadArgs]
	Base
}

// Thread outcomes, as counted.
const (
	threadDismissed = "dismissed"
	threadRestored  = "restored"
	threadIgnored   = "ignored"
	threadFailed    = "failed"
)

// Work implements river.Worker.
func (w *Thread) Work(ctx context.Context, job *river.Job[jobs.ThreadArgs]) error {
	args := job.Args
	file := w.Current.Get()
	account, err := w.account(file, args.AccountID)
	if err != nil {
		return err
	}
	logger := w.Logger.With("account", account.Key(), "pr", args.Number, "comment", args.CommentID,
		"resolved", args.Resolved, "sender", args.Sender)
	pr, err := loadPullRequest(ctx, w.Store, args.AccountID, args.RepositoryID, args.Number)
	if err != nil {
		return err
	}
	client, err := w.client(ctx, file, account, pr.repository)
	if err != nil {
		return err
	}
	outcome, err := w.apply(ctx, client, pr, args, logger)
	if err != nil {
		w.Metrics.Thread(account.Key(), threadFailed)
		logger.Error("thread change failed", "error", err)
		return err
	}
	w.Metrics.Thread(account.Key(), outcome)
	return nil
}

// apply dismisses or restores the finding the thread holds, when the
// sender may and the thread is one of the bot's finding threads, and says
// what it did.
func (w *Thread) apply(
	ctx context.Context, client forge.Client, pr *pullRequest, args jobs.ThreadArgs, logger *slog.Logger,
) (string, error) {
	owner, repo := pr.ownerRepo()
	perm, err := client.Permission(ctx, owner, repo, args.Sender)
	if err != nil {
		return "", fmt.Errorf("worker: look up the permission of %s: %w", args.Sender, err)
	}
	if !forge.CanWrite(perm) {
		logger.Info("thread change ignored", "reason", "sender has "+string(perm)+" access, write is required")
		return threadIgnored, nil
	}
	fingerprint, err := threadFinding(ctx, client, owner, repo, args.CommentID)
	if err != nil {
		return "", err
	}
	if fingerprint == "" {
		logger.Info("thread change ignored", "reason", "not a finding thread")
		return threadIgnored, nil
	}
	logger = logger.With("fingerprint", fingerprint)
	var found bool
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		if !args.Resolved {
			var err error
			found, err = store.DeleteDismissal(ctx, tx, pr.id, fingerprint)
			return err
		}
		finding, ok, err := store.LatestFinding(ctx, tx, pr.id, fingerprint)
		if err != nil || !ok {
			return err
		}
		found = true
		return store.RecordDismissal(ctx, tx, store.Dismissal{
			AccountID: args.AccountID, PullRequestID: pr.id, Fingerprint: fingerprint, Finding: finding,
			Reason: "thread resolved by " + args.Sender, Author: args.Sender, CommentID: args.CommentID,
		})
	})
	switch {
	case err != nil:
		return "", err
	case !found && args.Resolved:
		logger.Info("thread change ignored", "reason", "finding unknown")
		return threadIgnored, nil
	case !found:
		logger.Info("thread change ignored", "reason", "finding not dismissed")
		return threadIgnored, nil
	case args.Resolved:
		logger.Info("finding dismissed")
		return threadDismissed, nil
	default:
		logger.Info("finding restored")
		return threadRestored, nil
	}
}

// threadFinding is the fingerprint of the finding the thread opened by
// inline comment id holds: the one the bot's comment carries a
// FindingMarker for. "" when the thread is not one of the bot's finding
// threads.
func threadFinding(ctx context.Context, client forge.Client, owner, repo string, id int64) (string, error) {
	login, err := client.BotLogin(ctx)
	if err != nil {
		return "", err
	}
	root, err := client.GetComment(ctx, owner, repo, id, true)
	if err != nil {
		return "", fmt.Errorf("worker: read the comment opening the thread: %w", err)
	}
	if !strings.EqualFold(root.Author, login) {
		return "", nil
	}
	fingerprint, _ := review.MarkedFinding(root.Body)
	return fingerprint, nil
}
