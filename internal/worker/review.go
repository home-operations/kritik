// Package worker consumes River jobs. The review worker owns a review from
// the moment its job starts until write-back: it checks the head is still
// current, asks the forge for the merge-base, spawns a runner for the
// checkout work, and records everything about the run.
package worker

import (
	"context"
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
	"github.com/home-operations/kritik/internal/runner"
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

// Timeout implements river.Worker. River's default is one minute, far
// below a review: the runner Job may run to its deadline, then a lease
// wait and a model call follow. The Kubernetes deadline bounds the runner;
// this bounds the rest.
func (w *Review) Timeout(*river.Job[jobs.ReviewArgs]) time.Duration {
	return w.Deadline + jobTimeoutSlack
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
	mergeBase, err := client.MergeBase(ctx, owner, repo, pr.baseRef, pr.headSHA)
	if err != nil {
		return err
	}
	token, err := client.GitToken(ctx)
	if err != nil {
		return err
	}

	reviewID, runID, err := w.start(ctx, args, pr, mergeBase)
	if err != nil {
		return err
	}
	settings := file.Settings(tenant, pr.repository)
	deadline, resources := runnerSpec(tenant, w.Deadline)
	res := w.Executor.Run(ctx, executor.Spec{
		RunID: runID,
		Labels: map[string]string{
			"tenant": tenant.Slug, "repository": strings.ReplaceAll(pr.repository, "/", "_"),
			"pr": strconv.Itoa(args.Number), "kind": jobs.QueueReview,
		},
		Annotations: map[string]string{"river-job-id": strconv.FormatInt(job.ID, 10), "head-sha": args.HeadSHA},
		Params: runner.Params{
			RunID: runID, CloneURL: client.CloneURL(owner, repo), Token: token, Head: args.HeadSHA, Base: mergeBase, Ignore: settings.Ignore,
		},
		Deadline:  deadline,
		Resources: resources,
	})
	if err := recordRun(ctx, w.Store, w.Metrics, tenant.Slug, args.TenantID, runID, jobs.QueueReview, res); err != nil {
		return err
	}
	if res.Err != nil {
		logger.Warn("runner failed", "error", res.Err, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.Review(tenant.Slug, statusFailed, time.Since(started))
		return w.finishReview(ctx, args.TenantID, reviewID, statusFailed, "", res.Err.Error())
	}
	patchID, status, err := w.afterRun(ctx, args, pr, reviewID, runID, logger)
	if err != nil || patchID == "" {
		if err == nil {
			w.Metrics.Review(tenant.Slug, status, time.Since(started))
		}
		return err
	}
	phase := &publishPhase{
		w: w, file: file, tenant: tenant, settings: settings, client: client, pr: pr,
		reviewID: reviewID, runID: runID, jobID: job.ID, logger: logger,
	}
	status, perr := phase.run(ctx)
	if perr != nil && status == statusFailed {
		logger.Error("review failed", "error", perr)
	}
	logger.Info("review " + status)
	w.Metrics.Review(tenant.Slug, status, time.Since(started))
	return w.finishReview(ctx, args.TenantID, reviewID, status, patchID, errText(perr))
}

// afterRun re-checks the head under the tenant transaction and lifts the
// patch id out of the context pack. A bot-authored PR whose patch id equals
// its last prepared review is skipped: a Renovate rebase changes nothing.
// It returns the patch id when the review should go on to the model, and
// "" plus the terminal status it recorded otherwise.
func (w *Review) afterRun(
	ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, reviewID, runID string, logger *slog.Logger,
) (string, string, error) {
	status, patchID := statusPrepared, ""
	err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		var currentHead string
		if err := tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests WHERE id = $1`, pr.id).Scan(&currentHead); err != nil {
			return fmt.Errorf("worker: re-read head: %w", err)
		}
		if currentHead != args.HeadSHA {
			status = statusSuperseded
			return nil
		}
		if err := tx.QueryRow(ctx, `SELECT patch_id FROM context_packs WHERE runner_run_id = $1`, runID).Scan(&patchID); err != nil {
			return fmt.Errorf("worker: read context pack: %w", err)
		}
		if pr.authorIsBot {
			var last string
			err := tx.QueryRow(ctx, `SELECT patch_id FROM reviews WHERE pull_request_id = $1 AND id <> $2
				AND status IN ('prepared', 'completed') ORDER BY created_at DESC LIMIT 1`, pr.id, reviewID).Scan(&last)
			if err == nil && last == patchID {
				status = statusSkipped
			} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("worker: read last review: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if status != statusPrepared {
		logger.Info("review "+status, "patch_id", short(patchID))
		return "", status, w.finishReview(ctx, args.TenantID, reviewID, status, patchID, "")
	}
	// Prepared is not terminal: the model phase follows, so finished_at
	// stays NULL until it ends one way or the other.
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, patch_id = $3 WHERE id = $1`, reviewID, statusPrepared, patchID)
		return err
	})
	if err != nil {
		return "", "", fmt.Errorf("worker: mark review prepared: %w", err)
	}
	logger.Info("review prepared", "patch_id", short(patchID))
	return patchID, statusPrepared, nil
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

func (w *Review) start(ctx context.Context, args jobs.ReviewArgs, pr *pullRequest, mergeBase string) (reviewID, runID string, err error) {
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO reviews (tenant_id, pull_request_id, head_sha, merge_base_sha, status, trigger)
			VALUES ($1, $2, $3, $4, 'running', $5) RETURNING id`,
			args.TenantID, pr.id, args.HeadSHA, mergeBase, args.Trigger).Scan(&reviewID); err != nil {
			return fmt.Errorf("worker: insert review: %w", err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, review_id, kind) VALUES ($1, $2, 'review') RETURNING id`,
			args.TenantID, reviewID).Scan(&runID); err != nil {
			return fmt.Errorf("worker: insert runner run: %w", err)
		}
		return nil
	})
	return reviewID, runID, err
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
