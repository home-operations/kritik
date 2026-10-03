// Package worker consumes River jobs. The review worker owns a review from
// the moment its job starts until write-back: it checks the head is still
// current, asks the forge for the merge-base, spawns a runner for the
// checkout work, and records everything about the run.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"golang.org/x/sync/singleflight"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/executor"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/gitfetch"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/runner"
	"github.com/home-operations/kritika/internal/store"
)

// Review works the review queue.
type Review struct {
	river.WorkerDefaults[jobs.ReviewArgs]
	Base
	Executor executor.Executor
	// GatewayURL is where a runner calls its model, and GatewayTokenTTL how
	// long its run token outlives the Job's deadline.
	GatewayURL      string
	GatewayTokenTTL time.Duration
	// WebURL is the dashboard's origin, which the summary comment links
	// back to for a re-run; nil leaves the link out.
	WebURL *url.URL

	// superviseEvery overrides superviseInterval, and rowWait agentRowWait.
	superviseEvery time.Duration
	rowWait        time.Duration
}

// pullRequest is what the worker reads back before starting.
type pullRequest struct {
	id, repositoryID string
	repository       string
	number           int
	headSHA, baseRef string
	title, author    string
	authorIsBot      bool
}

// dedupesBotPatch says whether an unchanged patch from a bot author skips
// the review. A manual re-run bypasses the skip: the human asked for it, so
// an identical bot patch is reviewed again rather than deduped away.
func (pr *pullRequest) dedupesBotPatch(trigger string) bool {
	return pr.authorIsBot && trigger != jobs.TriggerManual
}

// lastPatchID is the patch id of the pull request's newest prepared or
// completed review other than reviewID, "" when it has none.
func lastPatchID(ctx context.Context, tx pgx.Tx, prID, reviewID string) (string, error) {
	var patch string
	err := tx.QueryRow(ctx, `SELECT patch_id FROM reviews WHERE pull_request_id = $1 AND id <> $2
		AND status IN ('prepared', 'completed') ORDER BY created_at DESC LIMIT 1`, prID, reviewID).Scan(&patch)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("worker: read last review: %w", err)
	}
	return patch, nil
}

// ownerRepo splits the repository's full name as the forge client takes it.
func (pr *pullRequest) ownerRepo() (owner, repo string) {
	owner, repo, _ = strings.Cut(pr.repository, "/")
	return owner, repo
}

// Work implements river.Worker.
func (w *Review) Work(ctx context.Context, job *river.Job[jobs.ReviewArgs]) error {
	args := job.Args
	file := w.Current.Get()
	account, err := w.account(file, args.AccountID)
	if err != nil {
		return err
	}
	logger := w.Logger.With("account", account.Key(), "pr", args.Number, "head", review.ShortSHA(args.HeadSHA))
	started := time.Now()

	b, done, err := w.begin(ctx, job, file, account, logger, started)
	if done {
		return err
	}
	pr, client, owner, repo, mergeBase, eff := b.early.pr, b.client, b.owner, b.repo, b.early.mergeBase, b.eff
	settings := eff.Settings
	admitted, done, err := w.admit(ctx, b.early, job, file, account, settings)
	if done {
		return err
	}
	if admitted.lease != nil {
		defer w.releaseLease(ctx, logger, admitted.lease, string(settings.Models.Review))
	}

	reviewID, runID, prior, err := w.start(ctx, args, pr, mergeBase, b.early.forgePatch, job.ID)
	if err != nil {
		return err
	}
	// The status says the review is under way until its outcome replaces
	// it, so a head is never silent while its runner works.
	if err := client.SetStatus(ctx, owner, repo, args.HeadSHA, forge.StatusPending, "kritika: review running"); err != nil {
		logger.Warn("commit status not set", "error", err)
	}
	deadline, resources := file.RunnerFor()
	spec := runner.Spec{
		Version: runner.SpecVersion, Kind: runner.KindReview, RunID: runID, CloneURL: client.CloneURL(owner, repo),
		Head: args.HeadSHA, Base: mergeBase, PriorHead: prior.headSHA, Ignore: settings.Ignore, RepoFiles: eff.repoFiles(),
		AgentFiles: eff.Review.AgentFiles,
	}
	secrets := runner.Secrets{GitToken: b.token}
	ended := endedReview{
		accountID: args.AccountID, accountKey: account.Key(), reviewID: reviewID, headSHA: args.HeadSHA,
		owner: owner, repo: repo, client: client, started: started, logger: logger,
	}
	deadline, promptNotes, err := w.agentSpec(ctx, args.AccountID, reviewID, runID, args.Trigger, pr, eff, prior, admitted, &spec, &secrets,
		deadline, client, logger)
	if err != nil {
		return w.agentSpecFailed(ctx, ended, runID, err)
	}
	b.notes = append(b.notes, promptNotes...)
	tools := file.ToolsFor(settings.Agent.Commands)
	sup := runSupervision(w.Store, args.AccountID, runID, pr.id, args.HeadSHA, w.superviseEvery, logger)
	res, cause := supervise(ctx, sup, w.Executor, executor.Spec{
		Labels: map[string]string{
			"account": account.Key(), "repository": pr.repository,
			"pr": strconv.Itoa(args.Number), "kind": jobs.QueueReview,
		},
		Annotations: map[string]string{"river-job-id": strconv.FormatInt(job.ID, 10), "head-sha": args.HeadSHA},
		Job:         spec,
		Secrets:     secrets,
		Deadline:    deadline,
		Resources:   resources,
		Tools:       tools,
	})
	// The agent's row is read before recordRun settles the run's phase: a
	// stopped run's row may still be on its way from the terminating pod.
	w.revokeGatewayTokens(ctx, logger, runID)
	agentOutcome, agentErr := w.readAgentRun(ctx, args.AccountID, runID, settings.Models.Review, stopped(ctx, res, cause))
	// A River cancel (JobCancelTx from a web request) cancels ctx itself,
	// unlike supervise's own errSuperseded/errHeartbeatLost, which only
	// cancel the child ctx passed to the executor. ctx is left live from here
	// on so a cancel that arrives during afterRun (review status "prepared")
	// still takes effect there. cctx is a detached copy for the terminal
	// writes up to afterRun, which must still land once ctx itself has
	// ended.
	canceled := errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely)
	cctx, cancel := detach(ctx)
	defer cancel()
	ended.jobName = res.JobName
	if err := recordRun(cctx, w.Store, w.Metrics, account.Key(), args.AccountID, runID, jobs.QueueReview, res); err != nil {
		if !canceled {
			return err
		}
		// River will not retry a canceled job, so the review ends here
		// whether or not its run's record could be written.
		logger.Error("runner run not recorded", "error", err)
	}
	// canceled takes priority over both agentErr and the run's own result: a
	// job River canceled must never be reported failed or retried, whether
	// or not the agent run record could be read. A run that finished without
	// a cancel is judged below by agentErr, then by its result; the head
	// check after that switch still catches a supersede that raced with a
	// normal finish.
	if canceled {
		return w.finishEnded(ctx, ended, nil)
	}
	// A run a stopping worker cut is retried, not judged: its failure, and
	// a missing agent row, are the stop's doing.
	if res.Err != nil && workerStopping(ctx) {
		return w.finishEnded(ctx, ended, res.Err)
	}
	if agentErr != nil {
		logger.Error("agent run not read", "error", agentErr)
		w.Metrics.Review(account.Key(), string(store.ReviewFailed), time.Since(started))
		// A retry would run the agent again; the review ends here.
		return w.failReview(cctx, ended, "", agentErr.Error())
	}
	switch {
	case res.Err != nil && errors.Is(cause, errSuperseded):
		logger.Info("review superseded while running", "job", res.JobName)
		w.Metrics.Review(account.Key(), string(store.ReviewSuperseded), time.Since(started))
		return w.finishReview(cctx, args.AccountID, reviewID, store.ReviewSuperseded, "", "")
	case res.Err != nil && errors.Is(cause, errHeartbeatLost):
		logger.Warn("runner heartbeat lost", "job", res.JobName)
		w.Metrics.Review(account.Key(), string(store.ReviewFailed), time.Since(started))
		return w.failReview(cctx, ended, "", "runner heartbeat lost")
	case res.Err != nil:
		logger.Warn("runner failed", "error", res.Err, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.Review(account.Key(), string(store.ReviewFailed), time.Since(started))
		return w.failReview(cctx, ended, "", res.Err.Error())
	}
	prep, status, err := w.afterRun(ctx, args, account, pr, eff, b.notes, client, reviewID, runID, prior, logger)
	if err != nil {
		// ctx stayed live through afterRun, so a cancel or a timeout that
		// arrived while it ran surfaces here as a plain error; the review
		// ends rather than staying running while River retries the job.
		return w.finishEnded(ctx, ended, err)
	}
	if prep.patchID == "" {
		w.Metrics.Review(account.Key(), string(status), time.Since(started))
		return nil
	}
	patchID := prep.patchID
	phase := &publishPhase{
		w: w, account: account, settings: prep.eff.Settings, client: client, pr: pr,
		reviewID: reviewID, runID: runID, trigger: args.Trigger, logger: logger,
		parse: review.ParseOptions{
			RequireSuggestedFix: prep.eff.Review.RequireSuggestedFix, Focused: prep.eff.Review.Focused(), Rules: prep.ruleIDs,
			Repository: pr.repository,
		},
		repoNotes: prep.notes, prior: prior, scope: prep.scope, templates: prep.templates,
		agent: agentOutcome,
	}
	status, perr := phase.run(ctx)
	// Publishing finishes on a detached ctx, so a clean result stands even
	// if ctx ended meanwhile: the comment and the commit status already say
	// so. Only a publish that failed while ctx ended ends as canceled or
	// timed out.
	if perr != nil && ctx.Err() != nil {
		return w.finishEnded(ctx, ended, perr)
	}
	if perr != nil && status == store.ReviewFailed {
		logger.Error("review failed", "error", perr)
	}
	logger.Info("review " + string(status))
	w.Metrics.Review(account.Key(), string(status), time.Since(started))
	fctx, fcancel := detach(ctx)
	defer fcancel()
	if status == store.ReviewFailed && !phase.statusReported {
		return w.failReview(fctx, ended, patchID, errText(perr))
	}
	return w.finishReview(fctx, args.AccountID, reviewID, status, patchID, errText(perr))
}

// prepared is what afterRun hands the publish phase: the patch id, the
// settings with the repository's .kritika.yaml applied and its templates
// read, the notes the summary states, whether the review builds on the
// last completed one, and the ids of the rules the agent was given.
type prepared struct {
	patchID   string
	eff       Effective
	templates review.Templates
	notes     []string
	scope     review.Scope
	ruleIDs   []string
}

// afterRun re-checks the head under the account transaction and reads the
// context pack: the runner decided whether the review is skipped and what
// it builds on, and read the merge-base repository files. A skipped
// review ends with a success status saying why. notes are the worker's
// own on the repository's file. It returns a patch id when the review
// should go on to publishing, and "" plus the terminal status it recorded
// otherwise.
func (w *Review) afterRun(
	ctx context.Context, args jobs.ReviewArgs, account *configfile.Account, pr *pullRequest, eff Effective, notes []string,
	client forge.Client, reviewID, runID string, prior priorReview, logger *slog.Logger,
) (prepared, store.ReviewStatus, error) {
	var pack store.ContextPackRecord
	var superseded bool
	err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		var currentHead string
		if err := tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE id = $1`, pr.id).Scan(&currentHead); err != nil {
			return fmt.Errorf("worker: re-read head: %w", err)
		}
		if currentHead != args.HeadSHA {
			superseded = true
			return nil
		}
		var err error
		pack, err = store.ReadContextPack(ctx, tx, runID)
		return err
	})
	if err != nil {
		return prepared{}, "", err
	}
	if superseded {
		logger.Info("review superseded", "patch_id", review.ShortSHA(pack.PatchID))
		return prepared{}, store.ReviewSuperseded, w.finishReview(ctx, args.AccountID, reviewID, store.ReviewSuperseded, pack.PatchID, "")
	}
	for stage, n := range pack.StageCounts {
		w.Metrics.ContextChunks(account.Key(), stage, n)
	}
	if pack.SkipReason != "" {
		logger.Info("review skipped", "reason", pack.SkipReason, "patch_id", review.ShortSHA(pack.PatchID))
		end := store.ReviewEnd{Status: store.ReviewSkipped, PatchID: pack.PatchID}
		if reason := repoconfig.SkipReason(pack.SkipReason); reason.Valid() {
			end.SkipReason = reason
		}
		if _, err := w.endReview(ctx, args.AccountID, reviewID, end); err != nil {
			return prepared{}, "", err
		}
		owner, repo := pr.ownerRepo()
		if err := client.SetStatus(ctx, owner, repo, args.HeadSHA, forge.StatusSuccess,
			"kritika: skipped ("+skipDescription(pack.SkipReason, eff.MaxChangedLines)+")"); err != nil {
			logger.Warn("commit status not set", "error", err)
		}
		return prepared{}, store.ReviewSkipped, nil
	}
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		return store.MarkReviewPrepared(ctx, tx, reviewID, pack.PatchID, pack.Scope, pack.ScopeReason, prior.id)
	})
	if err != nil {
		return prepared{}, "", err
	}
	logger.Info("review prepared", "patch_id", review.ShortSHA(pack.PatchID), "scope", pack.Scope, "scope_reason", pack.ScopeReason)
	return prepared{
		patchID: pack.PatchID, eff: eff, templates: eff.templates(pack.Files), notes: append(slices.Clone(notes), pack.Notes...),
		scope: pack.Scope, ruleIDs: pack.RuleIDs,
	}, store.ReviewPrepared, nil
}

// loadPullRequest reads a job's pull request. One the store does not know
// cancels the job: it will not appear by retrying.
func loadPullRequest(ctx context.Context, st *store.Store, accountID, repositoryID string, number int) (*pullRequest, error) {
	var pr pullRequest
	err := st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT p.id, p.repository_id, r.name, p.number, p.head_sha, p.base_ref, p.title, p.author, p.author_is_bot
			FROM pull_requests p JOIN repositories r ON r.id = p.repository_id
			WHERE p.repository_id = $1 AND p.number = $2`, repositoryID, number).
			Scan(&pr.id, &pr.repositoryID, &pr.repository, &pr.number,
				&pr.headSHA, &pr.baseRef, &pr.title, &pr.author, &pr.authorIsBot)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, river.JobCancel(fmt.Errorf("worker: pull request %d of %s is unknown", number, repositoryID))
	}
	if err != nil {
		return nil, fmt.Errorf("worker: load pull request: %w", err)
	}
	return &pr, nil
}

// earlyEnd is what a review ended before its runner is recorded with.
type earlyEnd struct {
	args                              jobs.ReviewArgs
	pr                                *pullRequest
	accountKey, mergeBase, forgePatch string
	// skip is why the repository's .kritika.yaml skipped the review.
	skip    repoconfig.SkipReason
	started time.Time
	logger  *slog.Logger
	// client reports the end on the head commit, under owner/repo; nil
	// until begin has built it, when a superseded head is the only end.
	client      forge.Client
	owner, repo string
}

// end records a review that never ran as status, for reason, counts it,
// and says so on the head commit, so a head no runner was spent on does
// not read as one still waiting for its review.
func (w *Review) end(ctx context.Context, e earlyEnd, status store.ReviewStatus, reason string) error {
	w.Metrics.Review(e.accountKey, string(status), time.Since(e.started))
	err := w.Store.WithAccount(ctx, e.args.AccountID, func(tx pgx.Tx) error {
		return store.RecordEndedReview(ctx, tx, store.EndedReview{
			AccountID: e.args.AccountID, PullRequestID: e.pr.id, HeadSHA: e.args.HeadSHA, MergeBaseSHA: e.mergeBase,
			ForgePatchID: e.forgePatch, Status: status, SkipReason: e.skip, Trigger: e.args.Trigger, Error: reason,
		})
	})
	if err != nil {
		return err
	}
	state, desc := e.status(status, reason)
	if state == "" || e.client == nil {
		return nil
	}
	if err := e.client.SetStatus(ctx, e.owner, e.repo, e.args.HeadSHA, state, desc); err != nil {
		e.logger.Warn("commit status not set", "error", err)
	}
	return nil
}

// status is the commit status an early end reports, or "" for none: a
// superseded head leaves the status to its successor.
func (e earlyEnd) status(status store.ReviewStatus, reason string) (forge.StatusState, string) {
	switch status {
	case store.ReviewFailed:
		return forge.StatusError, "kritika: review failed (" + reason + ")"
	case store.ReviewCapped:
		return forge.StatusSuccess, "kritika: capped (" + reason + ")"
	case store.ReviewSkipped:
		switch {
		case e.skip.Valid():
			reason = e.skip.Description()
		case reason == "":
			// The one skip with neither a repository reason nor an
			// admission reason is an unchanged bot patch.
			reason = skipDescription(runner.SkipUnchangedPatch, 0)
		}
		return forge.StatusSuccess, "kritika: skipped (" + reason + ")"
	}
	return "", ""
}

// skipUnchangedBot ends a bot's review whose rebase changed nothing,
// before a lease or a runner is spent on it; the runner's own patch check
// stays the backstop for what the forge cannot tell. A manual re-run is
// never skipped. It returns the forge patch id the review records, and
// whether it ended the review, with the error of recording that.
func (w *Review) skipUnchangedBot(ctx context.Context, e earlyEnd, client forge.Client, owner, repo string) (string, bool, error) {
	if !e.pr.authorIsBot || e.args.Trigger == jobs.TriggerManual {
		return "", false, nil
	}
	patch, unchanged := w.botPatch(ctx, e.logger, client, owner, repo, e.args.AccountID, e.pr, e.mergeBase)
	if !unchanged {
		return patch, false, nil
	}
	e.logger.Info("review skipped before its runner: bot patch unchanged", "forge_patch_id", review.ShortSHA(patch))
	e.forgePatch = patch
	return patch, true, w.end(ctx, e, store.ReviewSkipped, "")
}

// begun is a review job past everything before its admission: its pull
// request is current, a model slot was free when it looked, the forge
// answered, the repository's .kritika.yaml is applied and does not skip it,
// its settle time is over, and it is not an unchanged bot rebase. notes are
// what the review's summary says about the file.
type begun struct {
	early              earlyEnd
	eff                Effective
	notes              []string
	client             forge.Client
	owner, repo, token string
}

// begin takes a review job up to its admission, or ends it: superseded,
// snoozed while every model slot is held or until its settle time is over,
// or skipped by the merge-base .kritika.yaml or as an unchanged bot rebase.
// The admin's model's slots are checked before any forge call, so a job
// snoozed through a busy spell costs the forge nothing each time it wakes;
// a repository that chooses another model then waits for that model's
// slots too. It reports whether it ended the job, with the error of that or
// of getting this far.
func (w *Review) begin(
	ctx context.Context, job *river.Job[jobs.ReviewArgs], file *configfile.File, account *configfile.Account,
	logger *slog.Logger, started time.Time,
) (begun, bool, error) {
	args := job.Args
	pr, err := loadPullRequest(ctx, w.Store, args.AccountID, args.RepositoryID, args.Number)
	if err != nil {
		return begun{}, true, err
	}
	e := earlyEnd{args: args, pr: pr, accountKey: account.Key(), started: started, logger: logger}
	if pr.headSHA != args.HeadSHA {
		logger.Info("review superseded before start", "current_head", review.ShortSHA(pr.headSHA))
		return begun{}, true, w.end(ctx, e, store.ReviewSuperseded, "")
	}
	settings := file.Settings(account, pr.repository)
	if held, err := w.slotsHeld(ctx, e, job, account.ID(), settings); held {
		return begun{}, true, err
	}
	client, err := w.client(ctx, file, account, pr.repository)
	if err != nil {
		return begun{}, true, err
	}
	owner, repo := pr.ownerRepo()
	e.client, e.owner, e.repo = client, owner, repo
	if e.mergeBase, err = client.MergeBase(ctx, owner, repo, pr.baseRef, pr.headSHA); err != nil {
		return begun{}, true, err
	}
	doc, notes, err := readRepoConfig(ctx, client, owner, repo, e.mergeBase)
	if err != nil {
		return begun{}, true, err
	}
	eff, parseNotes := effective(settings, doc)
	if wait := settleLeft(args.Trigger, eff.Settle, job.CreatedAt, time.Now()); wait > 0 {
		logger.Info("review snoozed until its settle time is over", "for", wait.Round(time.Second))
		return begun{}, true, river.JobSnooze(wait)
	}
	if done, err := w.skipByRepo(ctx, e, &eff); done {
		return begun{}, true, err
	}
	if eff.Models.Review != settings.Models.Review {
		if held, err := w.slotsHeld(ctx, e, job, account.ID(), eff.Settings); held {
			return begun{}, true, err
		}
	}
	token, err := client.GitToken(ctx, repo)
	if err != nil {
		return begun{}, true, err
	}
	forgePatch, done, err := w.skipUnchangedBot(ctx, e, client, owner, repo)
	if done {
		return begun{}, true, err
	}
	e.forgePatch = forgePatch
	return begun{early: e, eff: eff, notes: append(notes, parseNotes...), client: client, owner: owner, repo: repo, token: token}, false, nil
}

// errNoSlot is a review's admission finding every model slot held after
// begin saw one free.
var errNoSlot = errors.New("worker: every model slot is held")

// admit settles what a review may spend before its runner starts: its
// runner spends against the model through the gateway, so it takes its
// model lease and passes the account's caps (see agentAdmit), and is
// snoozed if the slot begin saw free has been taken since. It reports
// whether it ended the job, with the error of admitting or of recording
// that.
func (w *Review) admit(
	ctx context.Context, e earlyEnd, job *river.Job[jobs.ReviewArgs], file *configfile.File, account *configfile.Account,
	settings configfile.Settings,
) (admission, bool, error) {
	a, status, reason, err := w.agentAdmit(ctx, e.logger, file, account, settings, job.ID)
	if errors.Is(err, errNoSlot) {
		return admission{}, true, w.snooze(e, job, string(settings.Models.Review))
	}
	if err != nil {
		return admission{}, true, err
	}
	if status == "" {
		return a, false, nil
	}
	e.logger.Warn("review "+string(status), "reason", reason)
	return admission{}, true, w.end(ctx, e, status, reason)
}

// slotsHeld snoozes the job when every one of the account's slots on the
// review model settings name is held, and reports whether it did, with the
// error that snoozes it. Slots it cannot count let the review go on.
func (w *Review) slotsHeld(
	ctx context.Context, e earlyEnd, job *river.Job[jobs.ReviewArgs], accountID string, settings configfile.Settings,
) (bool, error) {
	ref := string(settings.Models.Review)
	free, err := w.Store.SlotFree(ctx, accountID, ref, settings.Limits.Concurrency)
	if err != nil {
		e.logger.Warn("model slots not read; the review goes on", "error", err)
		return false, nil
	}
	if free {
		return false, nil
	}
	return true, w.snooze(e, job, ref)
}

// A review that has not started its runner is snoozed while every model
// slot is held, between snoozeMin and snoozeMax, and gives its worker back
// to the queue meanwhile.
const (
	snoozeMin = 5 * time.Second
	snoozeMax = 5 * time.Minute
)

// snooze puts a review that found every model slot held back on the
// queue, for longer each time, without counting an attempt: it gives its
// worker back rather than holding it while it waits. River keeps the
// count of a job's snoozes in its metadata.
func (w *Review) snooze(e earlyEnd, job *river.Job[jobs.ReviewArgs], modelKey string) error {
	var meta struct {
		Snoozes int `json:"snoozes"`
	}
	if err := json.Unmarshal(job.Metadata, &meta); err != nil {
		e.logger.Warn("job metadata not read; snoozing as if for the first time", "error", err)
	}
	d := store.Backoff(meta.Snoozes, snoozeMin, snoozeMax)
	e.logger.Info("review snoozed: every model slot is held", "model", modelKey, "snoozes", meta.Snoozes+1, "for", d.Round(time.Second))
	w.Metrics.ReviewSnoozed(e.accountKey, modelKey)
	return river.JobSnooze(d)
}

// botPatch is the patch id of a bot pull request's diff as its forge
// reports it, and whether the pull request's last prepared or completed
// review had the same one. A diff the forge will not give, or a review
// before it without one, tells nothing: the pull request is then reviewed
// as usual.
func (w *Review) botPatch(
	ctx context.Context, logger *slog.Logger, client forge.Client, owner, repo, accountID string, pr *pullRequest, mergeBase string,
) (patch string, unchanged bool) {
	diff, err := client.PullRequestDiff(ctx, owner, repo, mergeBase, pr.headSHA)
	if err != nil {
		logger.Warn("forge diff not read; the runner checks the patch", "error", err)
		return "", false
	}
	patch = gitfetch.PatchID(diff)
	var last string
	err = w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT forge_patch_id FROM reviews WHERE pull_request_id = $1
			AND status IN ('prepared', 'completed') ORDER BY created_at DESC LIMIT 1`, pr.id).Scan(&last)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logger.Warn("last forge patch id not read; the runner checks the patch", "error", err)
	}
	return patch, last != "" && last == patch
}

// start records the review and its runner run, and reads the last
// completed review the new one may build on.
func (w *Review) start(
	ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, mergeBase, forgePatchID string, jobID int64,
) (reviewID, runID string, prior priorReview, err error) {
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		var err error
		if prior, err = lastCompleted(ctx, tx, pr.id); err != nil {
			return err
		}
		dismissed, err := store.Dismissals(ctx, tx, pr.id)
		if err != nil {
			return err
		}
		prior.withDismissals(dismissed)
		reviewID, runID, err = store.StartReview(ctx, tx, store.NewReview{
			AccountID: args.AccountID, PullRequestID: pr.id, HeadSHA: args.HeadSHA, MergeBaseSHA: mergeBase,
			ForgePatchID: forgePatchID, Trigger: args.Trigger, JobID: jobID,
		})
		return err
	})
	return reviewID, runID, prior, err
}

// endReview ends a review as end says, and reports whether it did (see
// store.EndReview).
func (w *Review) endReview(ctx context.Context, accountID, reviewID string, end store.ReviewEnd) (bool, error) {
	var ended bool
	err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		ended, err = store.EndReview(ctx, tx, reviewID, end)
		return err
	})
	return ended, err
}

// finishReview ends a review as status, recording its patch id when known
// and why it failed.
func (w *Review) finishReview(ctx context.Context, accountID, reviewID string, status store.ReviewStatus, patchID, errText string) error {
	_, err := w.endReview(ctx, accountID, reviewID, store.ReviewEnd{Status: status, PatchID: patchID, Error: errText})
	return err
}

// failReview ends a review as failed and reports that on the head commit:
// a head whose review broke must not read as one still waiting for it.
// The description stays generic since the error can name internal hosts.
func (w *Review) failReview(ctx context.Context, e endedReview, patchID, errText string) error {
	if err := e.client.SetStatus(ctx, e.owner, e.repo, e.headSHA, forge.StatusError, "kritika: review failed"); err != nil {
		e.logger.Warn("commit status not set", "error", err)
	}
	return w.finishReview(ctx, e.accountID, e.reviewID, store.ReviewFailed, patchID, errText)
}

// endedReview is what finishEnded needs to know of the review whose job
// ended.
type endedReview struct {
	accountID, accountKey, reviewID, headSHA, jobName string
	owner, repo                                       string
	client                                            forge.Client
	started                                           time.Time
	logger                                            *slog.Logger
}

// finishEnded ends a review whose job ctx ended before the review could: a
// remote cancel as canceled, River's job timeout as failed, both returning
// nil, since an error would have River retry the review and pay for the
// model again. A stopping worker's cut is the exception: the review ends
// superseded by its retry, and an error is returned for River to retry it.
// A review that is already terminal is left as it is. While ctx is still
// live it returns err as is.
func (w *Review) finishEnded(ctx context.Context, e endedReview, err error) error {
	if ctx.Err() == nil {
		return err
	}
	cctx, cancel := detach(ctx)
	defer cancel()
	cause := context.Cause(ctx)
	if workerStopping(ctx) {
		if _, ferr := w.endReview(cctx, e.accountID, e.reviewID, store.ReviewEnd{
			Status: store.ReviewSuperseded, Error: "cut by a restart and retried", OnlyUnfinished: true,
		}); ferr != nil {
			return ferr
		}
		e.logger.Info("review cut by a restart; its job is retried", "job", e.jobName)
		w.Metrics.Review(e.accountKey, string(store.ReviewSuperseded), time.Since(e.started))
		return fmt.Errorf("worker: review cut by a restart: %w", cause)
	}
	status, errText, desc := store.ReviewCanceled, "", "kritika: review canceled"
	if !errors.Is(cause, river.ErrJobCancelledRemotely) {
		status, errText, desc = store.ReviewFailed, "review timed out: "+cause.Error(), "kritika: review timed out"
	}
	finished, ferr := w.endReview(cctx, e.accountID, e.reviewID, store.ReviewEnd{Status: status, Error: errText, OnlyUnfinished: true})
	if ferr != nil || !finished {
		return ferr
	}
	e.logger.Info("review "+string(status)+" as its job ended", "cause", cause, "job", e.jobName)
	if err := e.client.SetStatus(cctx, e.owner, e.repo, e.headSHA, forge.StatusError, desc); err != nil {
		e.logger.Warn("commit status not set", "error", err)
	}
	w.Metrics.Review(e.accountKey, string(status), time.Since(e.started))
	return nil
}

// agentSpecFailed ends a review whose runner never started because its
// agent spec could not be built, and the run made for it. When the job's
// ctx ended meanwhile (a remote cancel, River's timeout, a stopping
// worker), that is why, and the review ends as finishEnded ends it;
// otherwise err is returned for River to retry.
func (w *Review) agentSpecFailed(ctx context.Context, e endedReview, runID string, err error) error {
	dctx, cancel := detach(ctx)
	defer cancel()
	runErr := failRun(dctx, w.Store, e.accountID, runID, err.Error())
	if ctx.Err() != nil {
		if runErr != nil {
			e.logger.Warn("runner run not ended", "error", runErr)
		}
		return w.finishEnded(ctx, e, err)
	}
	return errors.Join(err, w.failReview(dctx, e, "", err.Error()), runErr)
}

// workerStopping reports whether ctx, a job's, ended because its River
// client is stopping: River otherwise ends a job's ctx only when an admin
// cancels the job or its timeout passes.
func workerStopping(ctx context.Context) bool {
	cause := context.Cause(ctx)
	return ctx.Err() != nil && !errors.Is(cause, river.ErrJobCancelledRemotely) && !errors.Is(cause, context.DeadlineExceeded)
}

// failRun ends a runner run that never got a Job.
func failRun(ctx context.Context, st *store.Store, accountID, runID, errText string) error {
	return st.WithAccount(ctx, accountID, func(tx pgx.Tx) error { return store.FailRunnerRun(ctx, tx, runID, errText) })
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ForgeCache is the forge.Clients over configured GitHub Apps. One client
// per App and repository owner, built on first use and kept for the life of
// the process, as the configuration is.
type ForgeCache struct {
	Build func(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error)

	mu      sync.Mutex
	clients map[string]forge.Client
	// building shares one build per key among the callers that find none
	// cached, outside mu: a build asks the forge, and a slow answer must
	// not hold up the callers of every other connection and owner.
	building singleflight.Group
}

// forgeBuildTimeout bounds a client's build, which runs on no caller's ctx.
const forgeBuildTimeout = time.Minute

// For implements forge.Clients. A caller whose ctx ends while a build is
// under way returns with ctx's error; the build goes on for the others,
// whichever caller started it.
func (c *ForgeCache) For(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error) {
	owner, _, _ := strings.Cut(repo, "/")
	key := in.Name + "/" + strings.ToLower(owner)
	c.mu.Lock()
	client, ok := c.clients[key]
	c.mu.Unlock()
	if ok {
		return client, nil
	}
	results := c.building.DoChan(key, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgeBuildTimeout)
		defer cancel()
		client, err := c.Build(bctx, in, repo)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.clients == nil {
			c.clients = map[string]forge.Client{}
		}
		c.clients[key] = client
		c.mu.Unlock()
		return client, nil
	})
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("worker: forge client for %s: %w", key, ctx.Err())
	case res := <-results:
		if res.Err != nil {
			return nil, res.Err
		}
		client, _ := res.Val.(forge.Client)
		return client, nil
	}
}
