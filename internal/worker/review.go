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
		return w.record(ctx, args, pr, statusSuperseded, "", "", "")
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

	settings := file.Settings(tenant, pr.repository)
	agentic := settings.Mode == configfile.ReviewAgentic
	// An agentic runner spends against the model itself, so the caps and
	// the model lease come before it rather than after.
	var admitted admission
	if agentic {
		a, status, reason, err := w.agentAdmit(ctx, logger, file, tenant, settings, job.ID)
		if err != nil {
			return err
		}
		if status != "" {
			logger.Warn("review "+status, "reason", reason)
			w.Metrics.Review(tenant.Slug, status, time.Since(started))
			return w.record(ctx, args, pr, status, mergeBase, "", reason)
		}
		admitted = a
		defer w.releaseLease(ctx, logger, a.lease, string(settings.Models.Review))
	}

	reviewID, runID, prior, err := w.start(ctx, args, pr, mergeBase, settings.Mode)
	if err != nil {
		return err
	}
	deadline, resources := runnerSpec(tenant, w.Deadline)
	spec := runner.Spec{
		Version: runner.SpecVersion, Kind: runner.KindReview, RunID: runID, CloneURL: client.CloneURL(owner, repo),
		Head: args.HeadSHA, Base: mergeBase, PriorHead: prior.headSHA, Ignore: settings.Ignore, RepoFiles: settings.Review.Referenced(),
	}
	secrets := runner.Secrets{GitToken: token}
	if agentic {
		if deadline, err = w.agentSpec(ctx, args.TenantID, reviewID, pr, settings, prior, admitted, &spec, &secrets, deadline); err != nil {
			return errors.Join(err, w.finishReview(ctx, args.TenantID, reviewID, statusFailed, "", err.Error()),
				failRun(ctx, w.Store, args.TenantID, runID, err.Error()))
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
	// The agent's spend is read before recordRun settles the run's phase:
	// a stopped run's row may still be on its way from the terminating pod.
	var agentOutcome *agentRun
	var chargeErr error
	if agentic {
		agentOutcome, chargeErr = w.chargeAgentRun(ctx, tenant, pr, reviewID, runID, settings.Models.Review, stopped(ctx, res, cause))
	}
	if err := recordRun(ctx, w.Store, w.Metrics, tenant.Slug, args.TenantID, runID, jobs.QueueReview, res); err != nil {
		return err
	}
	if chargeErr != nil {
		logger.Error("agent run not charged", "error", chargeErr)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		// A retry would run the agent again; the review ends here.
		return w.finishReview(ctx, args.TenantID, reviewID, statusFailed, "", chargeErr.Error())
	}
	// A run that finished despite a cancel is judged by its result; the
	// head check after it catches a supersede.
	switch {
	case res.Err != nil && errors.Is(cause, errSuperseded):
		logger.Info("review superseded while running", "job", res.JobName)
		w.Metrics.Review(tenant.Slug, statusSuperseded, time.Since(started))
		return w.finishReview(ctx, args.TenantID, reviewID, statusSuperseded, "", "")
	case res.Err != nil && errors.Is(cause, errHeartbeatLost):
		logger.Warn("runner heartbeat lost", "job", res.JobName)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		return w.finishReview(ctx, args.TenantID, reviewID, statusFailed, "", "runner heartbeat lost")
	case res.Err != nil:
		logger.Warn("runner failed", "error", res.Err, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		return w.finishReview(ctx, args.TenantID, reviewID, statusFailed, "", res.Err.Error())
	}
	prep, status, err := w.afterRun(ctx, args, pr, settings, client, reviewID, runID, prior, logger)
	if err != nil || prep.patchID == "" {
		if err == nil {
			w.Metrics.Review(tenant.Slug, status, time.Since(started))
		}
		return err
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
	if perr != nil && status == statusFailed {
		logger.Error("review failed", "error", perr)
	}
	logger.Info("review " + status)
	w.Metrics.Review(tenant.Slug, status, time.Since(started))
	return w.finishReview(ctx, args.TenantID, reviewID, status, patchID, errText(perr))
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
		if pr.authorIsBot {
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
func (w *Review) record(ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, status, mergeBase, patchID, errText string) error {
	return w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reviews
			(tenant_id, pull_request_id, head_sha, merge_base_sha, patch_id, status, trigger, error, finished_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
			args.TenantID, pr.id, args.HeadSHA, mergeBase, patchID, status, args.Trigger, errText)
		return err
	})
}

// start records the review and its runner run, and reads the last
// completed review the new one may build on.
func (w *Review) start(
	ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, mergeBase string, mode configfile.ReviewMode,
) (reviewID, runID string, prior priorReview, err error) {
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		var err error
		if prior, err = lastCompleted(ctx, tx, pr.id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO reviews (tenant_id, pull_request_id, head_sha, merge_base_sha, status, trigger, mode)
			VALUES ($1, $2, $3, $4, 'running', $5, $6) RETURNING id`,
			args.TenantID, pr.id, args.HeadSHA, mergeBase, args.Trigger, string(mode)).Scan(&reviewID); err != nil {
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
// client per installation, built on first use.
type ForgeCache struct {
	Build func(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error)

	mu      sync.Mutex
	clients map[string]forge.Client
}

// For implements Forges.
func (c *ForgeCache) For(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.clients[in.Name]; ok {
		return client, nil
	}
	client, err := c.Build(ctx, in, externalID, repo)
	if err != nil {
		return nil, err
	}
	if c.clients == nil {
		c.clients = map[string]forge.Client{}
	}
	c.clients[in.Name] = client
	return client, nil
}
