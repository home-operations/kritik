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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
)

// Forges builds and caches a forge client per installation. repo is a
// repository the installation can see, used to discover the installation
// id when the installation webhook has not recorded one yet.
type Forges interface {
	For(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error)
}

// Review works the review queue.
type Review struct {
	river.WorkerDefaults[jobs.ReviewArgs]
	Base
	Executor executor.Executor
	// Completers resolves the configured model providers.
	Completers CompleterSource
	// Embedder and EmbedModel enable stage 4 (similar code from the
	// repository's active index); nil Embedder skips it.
	Embedder   model.Embedder
	EmbedModel string
	// Deadline bounds a runner when the tenant sets none.
	Deadline time.Duration
	// GatewayURL is where an agentic runner calls its model, and
	// GatewayTokenTTL how long its run token outlives the Job's deadline.
	// Agentic reviews are refused without a gateway.
	GatewayURL      string
	GatewayTokenTTL time.Duration

	// superviseEvery overrides superviseInterval.
	superviseEvery time.Duration
}

// Review statuses the worker writes; the table's CHECK lists the same set.
const (
	statusPrepared   = "prepared"
	statusCompleted  = "completed"
	statusSuperseded = "superseded"
	statusSkipped    = "skipped"
	statusCapped     = "capped"
	statusFailed     = "failed"
	statusCanceled   = "canceled"
)

// pullRequest is what the worker reads back before starting.
type pullRequest struct {
	id, repositoryID, installationID string
	repository                       string
	number                           int
	installation                     string
	externalID                       int64
	headSHA, baseRef                 string
	authorIsBot                      bool
}

// Work implements river.Worker.
func (w *Review) Work(ctx context.Context, job *river.Job[jobs.ReviewArgs]) error {
	args := job.Args
	file := w.Current.Get()
	tenant, err := w.tenant(file, args.TenantID)
	if err != nil {
		return err
	}
	logger := w.Logger.With("tenant", tenant.Slug, "pr", args.Number, "head", short(args.HeadSHA))
	started := time.Now()

	pr, err := w.load(ctx, args)
	if err != nil {
		return err
	}
	if pr.headSHA != args.HeadSHA {
		logger.Info("review superseded before start", "current_head", short(pr.headSHA))
		w.Metrics.Review(tenant.Slug, statusSuperseded, time.Since(started))
		return w.record(ctx, args, pr, statusSuperseded, "", "", "", "")
	}
	client, err := w.client(ctx, file, pr.installation, pr.externalID, pr.repository)
	if err != nil {
		return err
	}
	owner, repo, _ := strings.Cut(pr.repository, "/")
	mergeBase, err := client.MergeBase(ctx, owner, repo, pr.number, pr.baseRef, pr.headSHA)
	if err != nil {
		return err
	}
	token, err := client.GitToken(ctx)
	if err != nil {
		return err
	}
	early := earlyEnd{args: args, pr: pr, tenantSlug: tenant.Slug, mergeBase: mergeBase, started: started, logger: logger}
	forgePatch, done, err := w.skipUnchangedBot(ctx, early, client, owner, repo)
	if done {
		return err
	}
	early.forgePatch = forgePatch

	settings := file.Settings(tenant, pr.repository)
	agentic := settings.Mode == configfile.ReviewAgentic
	// An agentic runner spends against the model itself, so the caps and
	// the model lease come before it rather than after.
	var admitted admission
	if agentic {
		a, done, err := w.admitAgentic(ctx, early, file, tenant, settings, job.ID)
		if done {
			return err
		}
		admitted = a
		defer w.releaseLease(ctx, logger, a.lease, string(settings.Models.Review))
	}

	reviewID, runID, prior, err := w.start(ctx, args, pr, mergeBase, forgePatch, settings.Mode, job.ID)
	if err != nil {
		return err
	}
	deadline, resources := runnerSpec(tenant, w.Deadline)
	spec := runner.Spec{
		Version: runner.SpecVersion, Kind: runner.KindReview, RunID: runID, CloneURL: client.CloneURL(owner, repo),
		Head: args.HeadSHA, Base: mergeBase, PriorHead: prior.headSHA, Ignore: settings.Ignore, RepoFiles: settings.Review.Referenced(),
	}
	secrets := runner.Secrets{GitToken: token}
	ended := endedReview{
		tenantID: args.TenantID, tenantSlug: tenant.Slug, reviewID: reviewID, headSHA: args.HeadSHA,
		owner: owner, repo: repo, client: client, started: started, logger: logger,
	}
	if agentic {
		deadline, err = w.agentSpec(ctx, args.TenantID, reviewID, runID, args.Trigger, pr, settings, prior, admitted, &spec, &secrets, deadline)
		if err != nil {
			return w.agentSpecFailed(ctx, ended, runID, err)
		}
	}
	sup := runSupervision(w.Store, args.TenantID, runID, pr.id, args.HeadSHA, w.superviseEvery, logger)
	res, cause := supervise(ctx, sup, w.Executor, executor.Spec{
		RunID: runID,
		Labels: map[string]string{
			"tenant": tenant.Slug, "repository": strings.ReplaceAll(pr.repository, "/", "_"),
			"pr": strconv.Itoa(args.Number), "kind": jobs.QueueReview,
		},
		Annotations: map[string]string{"river-job-id": strconv.FormatInt(job.ID, 10), "head-sha": args.HeadSHA},
		Job:         spec,
		Secrets:     secrets,
		Deadline:    deadline,
		Resources:   resources,
	})
	// The agent's row is read before recordRun settles the run's phase: a
	// stopped run's row may still be on its way from the terminating pod.
	var agentOutcome *agentRun
	var agentErr error
	if agentic {
		w.revokeGatewayTokens(ctx, logger, runID)
		agentOutcome, agentErr = w.readAgentRun(ctx, args.TenantID, runID, settings.Models.Review, stopped(ctx, res, cause))
	}
	// A River cancel (JobCancelTx from a web request) cancels ctx itself,
	// unlike supervise's own errSuperseded/errHeartbeatLost, which only
	// cancel the child ctx passed to the executor. ctx is left live from here
	// on so a cancel that arrives during afterRun/publish (review status
	// "prepared") still takes effect there, and a hung model call in publish
	// still respects River's job timeout. cctx is a detached copy for the
	// terminal writes below, which must still land once ctx itself has ended.
	canceled := errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely)
	cctx := context.WithoutCancel(ctx)
	ended.jobName = res.JobName
	if err := recordRun(cctx, w.Store, w.Metrics, tenant.Slug, args.TenantID, runID, jobs.QueueReview, res); err != nil {
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
	if agentErr != nil {
		logger.Error("agent run not read", "error", agentErr)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		// A retry would run the agent again; the review ends here.
		return w.finishReview(cctx, args.TenantID, reviewID, statusFailed, "", agentErr.Error())
	}
	switch {
	case res.Err != nil && errors.Is(cause, errSuperseded):
		logger.Info("review superseded while running", "job", res.JobName)
		w.Metrics.Review(tenant.Slug, statusSuperseded, time.Since(started))
		return w.finishReview(cctx, args.TenantID, reviewID, statusSuperseded, "", "")
	case res.Err != nil && errors.Is(cause, errHeartbeatLost):
		logger.Warn("runner heartbeat lost", "job", res.JobName)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		return w.finishReview(cctx, args.TenantID, reviewID, statusFailed, "", "runner heartbeat lost")
	case res.Err != nil:
		logger.Warn("runner failed", "error", res.Err, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		return w.finishReview(cctx, args.TenantID, reviewID, statusFailed, "", res.Err.Error())
	}
	prep, status, err := w.afterRun(ctx, args, pr, settings, client, reviewID, runID, prior, logger)
	if err != nil {
		// ctx stayed live through afterRun, so a cancel or a timeout that
		// arrived while it ran surfaces here as a plain error; the review
		// ends rather than staying running while River retries the job.
		return w.finishEnded(ctx, ended, err)
	}
	if prep.patchID == "" {
		w.Metrics.Review(tenant.Slug, status, time.Since(started))
		return nil
	}
	patchID := prep.patchID
	phase := &publishPhase{
		w: w, file: file, tenant: tenant, settings: prep.eff.Settings, client: client, pr: pr,
		reviewID: reviewID, runID: runID, jobID: job.ID, logger: logger,
		parse: review.ParseOptions{RequireSuggestedFix: prep.eff.RequireSuggestedFix}, templates: prep.eff.Templates,
		instructions: prep.eff.Instructions, repoNotes: prep.notes, prior: prior, scope: prep.scope, agent: agentOutcome,
	}
	publish := phase.run
	if agentic {
		publish = phase.runAgentic
	}
	status, perr := publish(ctx)
	// Once the model has answered, publish finishes on a detached ctx, so a
	// clean result stands even if ctx ended meanwhile: the comment and the
	// commit status already say so. Only a publish that failed while ctx
	// ended (the model call cut short) ends as canceled or timed out.
	if perr != nil && ctx.Err() != nil {
		return w.finishEnded(ctx, ended, perr)
	}
	if perr != nil && status == statusFailed {
		logger.Error("review failed", "error", perr)
	}
	logger.Info("review " + status)
	w.Metrics.Review(tenant.Slug, status, time.Since(started))
	return w.finishReview(cctx, args.TenantID, reviewID, status, patchID, errText(perr))
}

// prepared is what afterRun hands the model phase: the patch id, the
// settings with the repository's .kritik.yaml applied, the notes the
// summary states about that file, and whether the review builds on the
// last completed one.
type prepared struct {
	patchID string
	eff     Effective
	notes   []string
	scope   review.Scope
}

// afterRun re-checks the head under the tenant transaction, lifts the patch
// id and the merge-base repository files out of the context pack, and
// applies .kritik.yaml: a review it disables, filters out or whose changes
// its skip rule covers ends skipped with a success status saying why. A
// bot-authored PR whose patch id equals its last prepared review is skipped
// too: a Renovate rebase changes nothing. It returns a patch id when the
// review should go on to the model, and "" plus the terminal status it
// recorded otherwise.
func (w *Review) afterRun(
	ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, settings configfile.Settings, client forge.Client,
	reviewID, runID string, prior priorReview, logger *slog.Logger,
) (prepared, string, error) {
	var (
		patchID, lastPatch             string
		priorFetched                   *string
		superseded                     bool
		changed, repoNotes, deltaPaths []string
		filesJSON                      []byte
		vars                           map[string]any
	)
	err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		var currentHead string
		if err := tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE id = $1`, pr.id).Scan(&currentHead); err != nil {
			return fmt.Errorf("worker: re-read head: %w", err)
		}
		if currentHead != args.HeadSHA {
			superseded = true
			return nil
		}
		if err := tx.QueryRow(ctx, `SELECT patch_id, changed_paths, repo_files, repo_notes, prior_head_sha, delta_paths
			FROM context_packs WHERE runner_run_id = $1`, runID).
			Scan(&patchID, &changed, &filesJSON, &repoNotes, &priorFetched, &deltaPaths); err != nil {
			return fmt.Errorf("worker: read context pack: %w", err)
		}
		var err error
		if vars, err = filterVars(ctx, tx, pr.id); err != nil {
			return err
		}
		// A manual re-run bypasses this skip: the human asked for it, so an
		// identical bot patch is reviewed again rather than deduped away.
		if pr.authorIsBot && args.Trigger != jobs.TriggerManual {
			err := tx.QueryRow(ctx, `SELECT patch_id FROM reviews WHERE pull_request_id = $1 AND id <> $2
				AND status IN ('prepared', 'completed') ORDER BY created_at DESC LIMIT 1`, pr.id, reviewID).Scan(&lastPatch)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("worker: read last review: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return prepared{}, "", err
	}
	if superseded {
		logger.Info("review "+statusSuperseded, "patch_id", short(patchID))
		return prepared{}, statusSuperseded, w.finishReview(ctx, args.TenantID, reviewID, statusSuperseded, patchID, "")
	}

	var files repoconfig.Files
	if err := json.Unmarshal(filesJSON, &files); err != nil {
		return prepared{}, "", fmt.Errorf("worker: decode repository files: %w", err)
	}
	eff, notes := effective(settings, files, repoNotes)
	reason, ferr := eff.skip(vars, changed)
	if ferr != nil {
		logger.Warn("repository filter failed to evaluate", "error", ferr)
	}
	if reason != "" {
		logger.Info("review "+statusSkipped, "reason", reason, "patch_id", short(patchID))
		if err := w.finishSkipped(ctx, args.TenantID, reviewID, patchID, reason); err != nil {
			return prepared{}, "", err
		}
		owner, repo, _ := strings.Cut(pr.repository, "/")
		if err := client.SetStatus(ctx, owner, repo, args.HeadSHA, forge.StatusSuccess,
			"kritik: skipped ("+reason.Description()+")"); err != nil {
			logger.Warn("commit status not set", "error", err)
		}
		return prepared{}, statusSkipped, nil
	}
	if pr.authorIsBot && lastPatch != "" && lastPatch == patchID {
		logger.Info("review "+statusSkipped, "patch_id", short(patchID))
		return prepared{}, statusSkipped, w.finishReview(ctx, args.TenantID, reviewID, statusSkipped, patchID, "")
	}

	scope, scopeReason := review.DecideScope(prior.id != "", priorFetched != nil, len(deltaPaths), eff.Incremental.MaxDeltaFiles)
	var priorID *string
	if prior.id != "" {
		priorID = &prior.id
	}
	// Prepared is not terminal: the model phase follows, so finished_at
	// stays NULL until it ends one way or the other.
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, patch_id = $3, scope = $4, scope_reason = $5, prior_review_id = $6
			WHERE id = $1`, reviewID, statusPrepared, patchID, string(scope), scopeReason, priorID)
		return err
	})
	if err != nil {
		return prepared{}, "", fmt.Errorf("worker: mark review prepared: %w", err)
	}
	logger.Info("review prepared", "patch_id", short(patchID), "scope", scope, "scope_reason", scopeReason, "delta_paths", len(deltaPaths))
	return prepared{patchID: patchID, eff: eff, notes: notes, scope: scope}, statusPrepared, nil
}

// filterVars rebuilds the filter's pr variable from the stored pull request
// row, the same keys webhook.PullRequest.FilterVars gives ingest.
func filterVars(ctx context.Context, tx pgx.Tx, prID string) (map[string]any, error) {
	pr, err := loadFilterPR(ctx, tx, prID)
	if err != nil {
		return nil, err
	}
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

func (w *Review) load(ctx context.Context, args jobs.ReviewArgs) (*pullRequest, error) {
	var pr pullRequest
	err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT p.id, p.repository_id, r.installation_id, r.name, p.number, i.name, coalesce(i.external_id, 0),
				p.head_sha, p.base_ref, p.author_is_bot
			FROM pull_requests p JOIN repositories r ON r.id = p.repository_id JOIN installations i ON i.id = r.installation_id
			WHERE p.repository_id = $1 AND p.number = $2`, args.RepositoryID, args.Number).
			Scan(&pr.id, &pr.repositoryID, &pr.installationID, &pr.repository, &pr.number, &pr.installation, &pr.externalID,
				&pr.headSHA, &pr.baseRef, &pr.authorIsBot)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, river.JobCancel(fmt.Errorf("worker: pull request %d of %s is unknown", args.Number, args.RepositoryID))
	}
	if err != nil {
		return nil, fmt.Errorf("worker: load pull request: %w", err)
	}
	return &pr, nil
}

// record writes a review that never ran: superseded before start.
func (w *Review) record(
	ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, status, mergeBase, patchID, forgePatchID, errText string,
) error {
	return w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reviews
			(tenant_id, pull_request_id, head_sha, merge_base_sha, patch_id, forge_patch_id, status, trigger, error, finished_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`,
			args.TenantID, pr.id, args.HeadSHA, mergeBase, patchID, forgePatchID, status, args.Trigger, errText)
		return err
	})
}

// earlyEnd is what a review ended before its runner is recorded with.
type earlyEnd struct {
	args                              jobs.ReviewArgs
	pr                                *pullRequest
	tenantSlug, mergeBase, forgePatch string
	started                           time.Time
	logger                            *slog.Logger
}

// end records the review as status, for reason, and counts it.
func (w *Review) end(ctx context.Context, e earlyEnd, status, reason string) error {
	w.Metrics.Review(e.tenantSlug, status, time.Since(e.started))
	return w.record(ctx, e.args, e.pr, status, e.mergeBase, "", e.forgePatch, reason)
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
	patch, unchanged := w.botPatch(ctx, e.logger, client, owner, repo, e.args.TenantID, e.pr, e.mergeBase)
	if !unchanged {
		return patch, false, nil
	}
	e.logger.Info("review "+statusSkipped+" before its runner: bot patch unchanged", "forge_patch_id", short(patch))
	e.forgePatch = patch
	return patch, true, w.end(ctx, e, statusSkipped, "")
}

// admitAgentic admits an agentic review (see agentAdmit), and ends one it
// refuses. It reports whether it ended the review, with the error of
// admitting or of recording that.
func (w *Review) admitAgentic(
	ctx context.Context, e earlyEnd, file *configfile.File, tenant *configfile.Tenant, settings configfile.Settings, jobID int64,
) (admission, bool, error) {
	a, status, reason, err := w.agentAdmit(ctx, e.logger, file, tenant, settings, jobID)
	if err != nil {
		return admission{}, true, err
	}
	if status == "" {
		return a, false, nil
	}
	e.logger.Warn("review "+status, "reason", reason)
	return admission{}, true, w.end(ctx, e, status, reason)
}

// botPatch is the patch id of a bot pull request's diff as its forge
// reports it, and whether the pull request's last prepared or completed
// review had the same one. A diff the forge will not give, or a review
// before it without one, tells nothing: the pull request is then reviewed
// as usual.
func (w *Review) botPatch(
	ctx context.Context, logger *slog.Logger, client forge.Client, owner, repo, tenantID string, pr *pullRequest, mergeBase string,
) (patch string, unchanged bool) {
	diff, err := client.PullRequestDiff(ctx, owner, repo, pr.number, mergeBase, pr.headSHA)
	if err != nil {
		logger.Warn("forge diff not read; the runner checks the patch", "error", err)
		return "", false
	}
	patch = gitfetch.PatchID(diff)
	var last string
	err = w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
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
	ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, mergeBase, forgePatchID string, mode configfile.ReviewMode, jobID int64,
) (reviewID, runID string, prior priorReview, err error) {
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		var err error
		if prior, err = lastCompleted(ctx, tx, pr.id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO reviews
			(tenant_id, pull_request_id, head_sha, merge_base_sha, forge_patch_id, status, trigger, mode, river_job_id)
			VALUES ($1, $2, $3, $4, $5, 'running', $6, $7, $8) RETURNING id`,
			args.TenantID, pr.id, args.HeadSHA, mergeBase, forgePatchID, args.Trigger, string(mode), jobID).Scan(&reviewID); err != nil {
			return fmt.Errorf("worker: insert review: %w", err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, review_id, kind) VALUES ($1, $2, 'review') RETURNING id`,
			args.TenantID, reviewID).Scan(&runID); err != nil {
			return fmt.Errorf("worker: insert runner run: %w", err)
		}
		return nil
	})
	return reviewID, runID, prior, err
}

func (w *Review) finishReview(ctx context.Context, tenantID, reviewID, status, patchID, errText string) error {
	return w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, patch_id = $3, error = left($4, 2000), finished_at = now() WHERE id = $1`,
			reviewID, status, patchID, errText)
		if err != nil {
			return fmt.Errorf("worker: finish review: %w", err)
		}
		return nil
	})
}

// endedReview is what finishEnded needs to know of the review whose job
// ended.
type endedReview struct {
	tenantID, tenantSlug, reviewID, headSHA, jobName string
	owner, repo                                      string
	client                                           forge.Client
	started                                          time.Time
	logger                                           *slog.Logger
}

// finishEnded ends a review whose job ctx ended before the review could: a
// remote cancel as canceled, anything else (River's job timeout above all)
// as failed. Either way it returns nil, since an error would have River
// retry the review and pay for the model again. A review that is already
// terminal is left as it is. While ctx is still live it returns err as is.
func (w *Review) finishEnded(ctx context.Context, e endedReview, err error) error {
	if ctx.Err() == nil {
		return err
	}
	cctx, cancel := detach(ctx)
	defer cancel()
	cause := context.Cause(ctx)
	status, errText, desc := statusCanceled, "", "kritik: review canceled"
	if !errors.Is(cause, river.ErrJobCancelledRemotely) {
		status, errText, desc = statusFailed, "review timed out: "+cause.Error(), "kritik: review timed out"
	}
	finished, ferr := w.finishUnfinished(cctx, e.tenantID, e.reviewID, status, errText)
	if ferr != nil || !finished {
		return ferr
	}
	e.logger.Info("review "+status+" as its job ended", "cause", cause, "job", e.jobName)
	if err := e.client.SetStatus(cctx, e.owner, e.repo, e.headSHA, forge.StatusError, desc); err != nil {
		e.logger.Warn("commit status not set", "error", err)
	}
	w.Metrics.Review(e.tenantSlug, status, time.Since(e.started))
	return nil
}

// agentSpecFailed ends a review whose runner never started because its
// agent spec could not be built, and the run made for it. When the job's
// ctx ended meanwhile (a remote cancel, River's timeout), that is why, and
// the review ends as finishEnded ends it, with no retry; otherwise err is
// returned for River to retry.
func (w *Review) agentSpecFailed(ctx context.Context, e endedReview, runID string, err error) error {
	dctx, cancel := detach(ctx)
	defer cancel()
	runErr := failRun(dctx, w.Store, e.tenantID, runID, err.Error())
	if ctx.Err() != nil {
		if runErr != nil {
			e.logger.Warn("runner run not ended", "error", runErr)
		}
		return w.finishEnded(ctx, e, err)
	}
	return errors.Join(err, w.finishReview(dctx, e.tenantID, e.reviewID, statusFailed, "", err.Error()), runErr)
}

// finishUnfinished ends a review only if nothing has ended it yet, and
// reports whether it did.
func (w *Review) finishUnfinished(ctx context.Context, tenantID, reviewID, status, errText string) (bool, error) {
	var finished bool
	err := w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, error = left($3, 2000), finished_at = now()
			WHERE id = $1 AND finished_at IS NULL`, reviewID, status, errText)
		if err != nil {
			return fmt.Errorf("worker: finish review: %w", err)
		}
		finished = tag.RowsAffected() == 1
		return nil
	})
	return finished, err
}

// failRun ends a runner run that never got a Job, so it does not stay
// 'created' for good.
func failRun(ctx context.Context, st *store.Store, tenantID, runID, errText string) error {
	return st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = 'failed', error = left($2, 2000), finished_at = now() WHERE id = $1`,
			runID, errText)
		if err != nil {
			return fmt.Errorf("worker: fail runner run: %w", err)
		}
		return nil
	})
}

func (w *Review) finishSkipped(ctx context.Context, tenantID, reviewID, patchID string, reason repoconfig.SkipReason) error {
	return w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, patch_id = $3, skip_reason = $4, finished_at = now() WHERE id = $1`,
			reviewID, statusSkipped, patchID, string(reason))
		if err != nil {
			return fmt.Errorf("worker: finish review: %w", err)
		}
		return nil
	})
}

func tenantByID(f *configfile.File, id string) *configfile.Tenant {
	for i := range f.Tenants {
		if f.Tenants[i].ID() == id {
			return &f.Tenants[i]
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ForgeCache is the Forges implementation over configured GitHub Apps. One
// client per installation, built on first use and rebuilt when the
// installation's credentials change.
type ForgeCache struct {
	Build func(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error)

	mu      sync.Mutex
	clients map[string]cachedForge
}

type cachedForge struct {
	fingerprint string
	client      forge.Client
}

// For implements Forges.
func (c *ForgeCache) For(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error) {
	fp := credentialFingerprint(in)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.clients[in.Name]; ok && cached.fingerprint == fp {
		return cached.client, nil
	}
	client, err := c.Build(ctx, in, externalID, repo)
	if err != nil {
		return nil, err
	}
	if c.clients == nil {
		c.clients = map[string]cachedForge{}
	}
	c.clients[in.Name] = cachedForge{fingerprint: fp, client: client}
	return client, nil
}
