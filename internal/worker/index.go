package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
)

// embedBatch is how many staged chunks are embedded and inserted at once.
const embedBatch = 128

// Index modes, as index_runs and index_packs spell them.
const (
	modeFull        = "full"
	modeIncremental = "incremental"
)

// Index works the index queue: one job builds or advances a repository's
// embedding index to a commit. The runner Job chunks the tree; this worker
// embeds the chunks and swaps or advances the active generation.
type Index struct {
	river.WorkerDefaults[jobs.IndexArgs]
	Base
	Executor executor.Executor
	Embedder model.Embedder
	// EmbedModel and EmbedDims name the deployment embedder; a generation
	// built with anything else is rebuilt in full.
	EmbedModel string
	EmbedDims  int
	Deadline   time.Duration

	// superviseEvery overrides superviseInterval.
	superviseEvery time.Duration
}

type indexRepo struct {
	name, installation, defaultBranch string
	externalID                        int64
	enabled                           bool
	activeRun                         string
}

type activeGeneration struct {
	id, commit, model string
	dims              int
}

// Work implements river.Worker.
func (w *Index) Work(ctx context.Context, job *river.Job[jobs.IndexArgs]) error {
	args := job.Args
	if w.Embedder == nil {
		return river.JobCancel(errors.New("worker: no embedder is configured, indexing is off"))
	}
	file := w.Current.Get()
	tenant, err := w.tenant(file, args.TenantID)
	if err != nil {
		return err
	}
	repo, err := w.loadRepo(ctx, args)
	if err != nil {
		return err
	}
	logger := w.Logger.With("tenant", tenant.Slug, "repository", repo.name, "trigger", args.Trigger)
	settings := file.Settings(tenant, repo.name)
	if !repo.enabled || !settings.Enabled {
		logger.Info("index skipped, repository disabled")
		return nil
	}
	client, err := w.client(ctx, file, repo.installation, repo.externalID, repo.name)
	if err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo.name, "/")
	commit := args.CommitSHA
	if commit == "" {
		branch := ""
		if commit, branch, err = client.BranchTip(ctx, owner, name, repo.defaultBranch); err != nil {
			return err
		}
		if repo.defaultBranch == "" {
			_ = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE repositories SET default_branch = $2 WHERE id = $1 AND default_branch = ''`, args.RepositoryID, branch)
				return err
			})
		}
	}
	logger = logger.With("commit", short(commit))

	active, err := w.activeGeneration(ctx, args.TenantID, repo.activeRun)
	if err != nil {
		return err
	}
	mode, base := modeFull, ""
	if active != nil && active.model == w.EmbedModel && active.dims == w.EmbedDims {
		if active.commit == commit {
			logger.Info("index already at this commit")
			return nil
		}
		mode, base = modeIncremental, active.commit
	}
	token, err := client.GitToken(ctx)
	if err != nil {
		return err
	}
	runID, runnerRunID, err := w.start(ctx, args, commit, base, mode)
	if err != nil {
		return err
	}
	deadline, resources := runnerSpec(tenant, w.Deadline)
	sup := runSupervision(w.Store, args.TenantID, runnerRunID, "", "", w.superviseEvery, logger)
	res, cause := supervise(ctx, sup, w.Executor, executor.Spec{
		RunID: runnerRunID,
		Labels: map[string]string{
			"tenant": tenant.Slug, "repository": strings.ReplaceAll(repo.name, "/", "_"), "kind": jobs.QueueIndex,
		},
		Annotations: map[string]string{"river-job-id": strconv.FormatInt(job.ID, 10), "head-sha": commit},
		Job: runner.Spec{
			Version: runner.SpecVersion, Kind: runner.KindIndex, RunID: runnerRunID, CloneURL: client.CloneURL(owner, name),
			Head: commit, Base: base, Ignore: settings.Ignore,
		},
		Secrets:  runner.Secrets{GitToken: token},
		Deadline: deadline, Resources: resources,
	})
	if err := recordRun(ctx, w.Store, w.Metrics, tenant.Slug, args.TenantID, runnerRunID, jobs.QueueIndex, res); err != nil {
		return err
	}
	if res.Err != nil {
		reason := res.Err.Error()
		if errors.Is(cause, errHeartbeatLost) {
			reason = "runner heartbeat lost"
		}
		logger.Warn("index runner failed", "error", reason, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.IndexRun(tenant.Slug, mode, "failed", 0)
		return w.finish(ctx, args.TenantID, runID, "failed", 0, reason)
	}
	n, mode, err := w.embed(ctx, args, tenant, commit, runID, runnerRunID, active, settings, job.ID)
	if err != nil {
		logger.Error("index embedding failed", "error", err)
		w.Metrics.IndexRun(tenant.Slug, mode, "failed", 0)
		_ = w.finish(ctx, args.TenantID, runID, "failed", 0, err.Error())
		return err
	}
	logger.Info("index completed", "mode", mode, "chunks", n)
	w.Metrics.IndexRun(tenant.Slug, mode, "completed", n)
	return w.finish(ctx, args.TenantID, runID, "completed", n, "")
}

func (w *Index) loadRepo(ctx context.Context, args jobs.IndexArgs) (*indexRepo, error) {
	var r indexRepo
	var active *string
	err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT r.name, i.name, coalesce(i.external_id, 0), r.default_branch, r.enabled, r.active_index_run_id::text
			FROM repositories r JOIN installations i ON i.id = r.installation_id WHERE r.id = $1`, args.RepositoryID).
			Scan(&r.name, &r.installation, &r.externalID, &r.defaultBranch, &r.enabled, &active)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, river.JobCancel(fmt.Errorf("worker: repository %s is unknown", args.RepositoryID))
	}
	if err != nil {
		return nil, fmt.Errorf("worker: load repository: %w", err)
	}
	if active != nil {
		r.activeRun = *active
	}
	return &r, nil
}

func (w *Index) activeGeneration(ctx context.Context, tenantID, runID string) (*activeGeneration, error) {
	if runID == "" {
		return nil, nil
	}
	g := &activeGeneration{id: runID}
	err := w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT commit_sha, embed_model, embed_dims FROM index_runs WHERE id = $1 AND status = 'completed'`, runID).
			Scan(&g.commit, &g.model, &g.dims)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: read active generation: %w", err)
	}
	return g, nil
}

func (w *Index) start(ctx context.Context, args jobs.IndexArgs, commit, base, mode string) (runID, runnerRunID string, err error) {
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO index_runs
			(tenant_id, repository_id, commit_sha, base_sha, embed_model, embed_dims, mode, status, trigger)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'running', $8) RETURNING id`,
			args.TenantID, args.RepositoryID, commit, base, w.EmbedModel, w.EmbedDims, mode, args.Trigger).Scan(&runID); err != nil {
			return fmt.Errorf("worker: insert index run: %w", err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO runner_runs (tenant_id, index_run_id, kind) VALUES ($1, $2, 'index') RETURNING id`,
			args.TenantID, runID).Scan(&runnerRunID); err != nil {
			return fmt.Errorf("worker: insert runner run: %w", err)
		}
		return nil
	})
	return runID, runnerRunID, err
}

func (w *Index) finish(ctx context.Context, tenantID, runID, status string, chunks int, errText string) error {
	return w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE index_runs SET status = $2, chunk_count = $3, error = left($4, 2000), finished_at = now() WHERE id = $1`,
			runID, status, chunks, errText)
		if err != nil {
			return fmt.Errorf("worker: finish index run: %w", err)
		}
		return nil
	})
}

type indexPack struct {
	mode, base   string
	changedPaths []string
	chunkCount   int
}

type stagedChunk struct {
	id                                        int64
	path, language, symbol, kind, scope, text string
	startLine, endLine                        int
}

// embed turns the staged chunks into index_chunks rows. A full build fills
// the new generation and then makes it active; an incremental step
// replaces the changed paths inside the active generation. Either way the
// swap is one transaction, so a review never sees a half-built index.
func (w *Index) embed(
	ctx context.Context, args jobs.IndexArgs, tenant *configfile.Tenant, commit, runID, runnerRunID string, active *activeGeneration,
	settings configfile.Settings, jobID int64,
) (int, string, error) {
	var pack indexPack
	err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mode, base_sha, changed_paths, chunk_count FROM index_packs WHERE runner_run_id = $1`, runnerRunID).
			Scan(&pack.mode, &pack.base, &pack.changedPaths, &pack.chunkCount)
	})
	if err != nil {
		return 0, "", fmt.Errorf("worker: read index pack: %w", err)
	}
	target := runID
	if pack.mode == modeIncremental && active != nil {
		target = active.id
	}
	// The runner may have fallen back to a full build; the run row says
	// what actually happened.
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE index_runs SET mode = $2, base_sha = $3 WHERE id = $1`, runID, pack.mode, pack.base)
		return err
	})
	if err != nil {
		return 0, pack.mode, fmt.Errorf("worker: record index mode: %w", err)
	}
	var total int
	var tokens int64
	err = w.withLease(ctx, tenant, "embed:"+w.EmbedModel, settings.Slots(), jobID, func(ctx context.Context) error {
		return w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
			if pack.mode == modeIncremental && len(pack.changedPaths) > 0 {
				if _, err := tx.Exec(ctx, `DELETE FROM index_chunks WHERE index_run_id = $1 AND path = ANY($2)`,
					target, pack.changedPaths); err != nil {
					return fmt.Errorf("worker: drop stale chunks: %w", err)
				}
			}
			var last int64
			for {
				batch, err := readStaged(ctx, tx, runnerRunID, last, embedBatch)
				if err != nil {
					return err
				}
				if len(batch) == 0 {
					break
				}
				texts := make([]string, len(batch))
				for i, c := range batch {
					texts[i] = embedText(c)
				}
				vectors, used, err := w.Embedder.Embed(ctx, texts)
				if err != nil {
					w.Metrics.ModelCall(tenant.Slug, w.EmbedModel, roleEmbedding, "error", 0, 0, 0, 0)
					return err
				}
				w.Metrics.ModelCall(tenant.Slug, w.EmbedModel, roleEmbedding, "ok", used, 0, 0, 0)
				tokens += used
				if err := insertChunks(ctx, tx, args.TenantID, args.RepositoryID, target, batch, vectors); err != nil {
					return err
				}
				total += len(batch)
				last = batch[len(batch)-1].id
			}
			if pack.mode == modeIncremental && active != nil {
				if _, err := tx.Exec(ctx, `UPDATE index_runs SET commit_sha = $2 WHERE id = $1`, active.id, commit); err != nil {
					return fmt.Errorf("worker: advance generation: %w", err)
				}
			} else {
				if _, err := tx.Exec(ctx, `UPDATE repositories SET active_index_run_id = $2 WHERE id = $1`, args.RepositoryID, runID); err != nil {
					return fmt.Errorf("worker: activate generation: %w", err)
				}
				if active != nil {
					if _, err := tx.Exec(ctx, `UPDATE index_runs SET status = 'superseded', finished_at = coalesce(finished_at, now())
					WHERE id = $1`, active.id); err != nil {
						return fmt.Errorf("worker: supersede generation: %w", err)
					}
					if _, err := tx.Exec(ctx, `DELETE FROM index_chunks WHERE index_run_id = $1`, active.id); err != nil {
						return fmt.Errorf("worker: drop superseded chunks: %w", err)
					}
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM index_staging WHERE runner_run_id = $1`, runnerRunID); err != nil {
				return fmt.Errorf("worker: clear staging: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO usage (tenant_id, repository_id, role, model, input_tokens) VALUES ($1, $2, 'embedding', $3, $4)`,
				args.TenantID, args.RepositoryID, w.EmbedModel, tokens); err != nil {
				return fmt.Errorf("worker: record embedding usage: %w", err)
			}
			return nil
		})
	})
	return total, pack.mode, err
}

func readStaged(ctx context.Context, tx pgx.Tx, runnerRunID string, after int64, limit int) ([]stagedChunk, error) {
	rows, err := tx.Query(ctx, `SELECT id, path, start_line, end_line, language, symbol, kind, scope, text
		FROM index_staging WHERE runner_run_id = $1 AND id > $2 ORDER BY id LIMIT $3`, runnerRunID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("worker: read staged chunks: %w", err)
	}
	defer rows.Close()
	var out []stagedChunk
	for rows.Next() {
		var c stagedChunk
		if err := rows.Scan(&c.id, &c.path, &c.startLine, &c.endLine, &c.language, &c.symbol, &c.kind, &c.scope, &c.text); err != nil {
			return nil, fmt.Errorf("worker: scan staged chunk: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// embedText is what the embedder sees: the path and symbol give the
// vector a little of the file's identity, the way a reader would know
// where a snippet came from.
func embedText(c stagedChunk) string {
	head := c.path
	if c.symbol != "" {
		head += " " + c.kind + " " + c.symbol
	}
	return head + "\n" + c.text
}

func insertChunks(ctx context.Context, tx pgx.Tx, tenantID, repositoryID, runID string, batch []stagedChunk, vectors [][]float32) error {
	if len(vectors) != len(batch) {
		return fmt.Errorf("worker: %d vectors for %d chunks", len(vectors), len(batch))
	}
	n := len(batch)
	strs := func() []string { return make([]string, n) }
	paths, langs, symbols, kinds, scopes, texts, embeddings := strs(), strs(), strs(), strs(), strs(), strs(), strs()
	starts, ends := make([]int32, n), make([]int32, n)
	for i, c := range batch {
		paths[i], langs[i], symbols[i], kinds[i], scopes[i], texts[i] = c.path, c.language, c.symbol, c.kind, c.scope, c.text
		starts[i], ends[i] = int32(c.startLine), int32(c.endLine)
		embeddings[i] = model.VectorLiteral(vectors[i])
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO index_chunks
			(tenant_id, repository_id, index_run_id, path, start_line, end_line, language, symbol, kind, scope, text, embedding)
		SELECT $1, $2, $3, c.path, c.start_line, c.end_line, c.language, c.symbol, c.kind, c.scope, c.text, c.embedding::halfvec
		FROM unnest($4::text[], $5::int[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::text[])
			AS c (path, start_line, end_line, language, symbol, kind, scope, text, embedding)`,
		tenantID, repositoryID, runID, paths, starts, ends, langs, symbols, kinds, scopes, texts, embeddings)
	if err != nil {
		return fmt.Errorf("worker: insert chunks: %w", err)
	}
	return nil
}

// recordRun writes what the executor learned about a runner Job and counts
// it.
func recordRun(
	ctx context.Context, st *store.Store, m *metrics.Metrics, tenant, tenantID, runID, kind string, res executor.Result,
) error {
	outcome := runOutcome(res)
	var took time.Duration
	if !res.StartedAt.IsZero() {
		took = time.Since(res.StartedAt)
	}
	m.RunnerRun(tenant, kind, outcome, took)
	return st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET job_name = $2, pod_name = $3, node_name = $4,
			scheduled_at = nullif($5, '0001-01-01'::timestamptz), started_at = nullif($6, '0001-01-01'::timestamptz), finished_at = now(),
			exit_code = $7, termination_reason = $8, deadline_exceeded = $9, log_tail = $10,
			phase = CASE WHEN $11 THEN phase ELSE 'failed' END, error = CASE WHEN $11 THEN error ELSE left($12, 2000) END
			WHERE id = $1`,
			runID, res.JobName, res.PodName, res.NodeName, res.ScheduledAt, res.StartedAt,
			res.ExitCode, res.TerminationReason, res.DeadlineExceeded, res.LogTail, res.Err == nil, errText(res.Err))
		if err != nil {
			return fmt.Errorf("worker: record runner run: %w", err)
		}
		return nil
	})
}

// runOutcome names how a runner Job ended for the metric label.
func runOutcome(res executor.Result) string {
	switch {
	case res.DeadlineExceeded:
		return "deadline"
	case res.Err != nil:
		return "failed"
	default:
		return "success"
	}
}

// runnerSpec applies the tenant's runner block over the deployment default.
func runnerSpec(tenant *configfile.Tenant, deadline time.Duration) (time.Duration, map[string]any) {
	var resources map[string]any
	if tenant.Runner != nil {
		if tenant.Runner.ActiveDeadlineSeconds > 0 {
			deadline = time.Duration(tenant.Runner.ActiveDeadlineSeconds) * time.Second
		}
		resources = tenant.Runner.Resources
	}
	return deadline, resources
}
