package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
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
	// Embedders resolves the instance's embedder; a generation built with
	// another model or dimension is rebuilt in full.
	Embedders *Embedders

	// superviseEvery overrides superviseInterval.
	superviseEvery time.Duration
	// batch overrides embedBatch.
	batch int
}

type indexRepo struct {
	name, defaultBranch string
	enabled             bool
	activeRun           string
}

type activeGeneration struct {
	id, commit, model string
	dims              int
}

// Work implements river.Worker.
func (w *Index) Work(ctx context.Context, job *river.Job[jobs.IndexArgs]) error {
	args := job.Args
	file := w.Current.Get()
	embedder, emb := w.Embedders.Embedder(file)
	if embedder == nil {
		return river.JobCancel(errors.New("worker: no embedder is configured, indexing is off"))
	}
	account, err := w.account(file, args.AccountID)
	if err != nil {
		return err
	}
	repo, err := w.loadRepo(ctx, args)
	if err != nil {
		return err
	}
	logger := w.Logger.With("account", account.Key(), "repository", repo.name, "trigger", args.Trigger)
	settings := file.Settings(account, repo.name)
	if !repo.enabled || !settings.Enabled {
		logger.Info("index skipped, repository disabled")
		return nil
	}
	client, err := w.client(ctx, file, account, repo.name)
	if err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo.name, "/")
	// The job indexes the tip, not the commit of the push that queued it:
	// pushes while it waited were absorbed into it.
	commit, branch, err := client.BranchTip(ctx, owner, name, repo.defaultBranch)
	if err != nil {
		return err
	}
	if repo.defaultBranch == "" {
		_ = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE repositories SET default_branch = $2 WHERE id = $1 AND default_branch = ''`, args.RepositoryID, branch)
			return err
		})
	}
	logger = logger.With("commit", short(commit))

	active, err := w.activeGeneration(ctx, args.AccountID, repo.activeRun)
	if err != nil {
		return err
	}
	mode, base := modeFull, ""
	if !args.Full && active != nil && active.model == emb.Model && active.dims == emb.Dims {
		if active.commit == commit {
			logger.Info("index already at this commit")
			return nil
		}
		mode, base = modeIncremental, active.commit
	}
	// The repository's own .kritik.yaml, as of the commit indexed, can stop
	// indexing and add ignore globs.
	doc, _, err := readRepoConfig(ctx, client, owner, name, commit)
	if err != nil {
		return err
	}
	eff, _ := effective(settings, doc)
	if !eff.Enabled {
		logger.Info("index skipped, disabled in .kritik.yaml")
		return nil
	}
	token, err := client.GitToken(ctx)
	if err != nil {
		return err
	}
	runID, runnerRunID, err := w.start(ctx, args, emb, commit, base, mode)
	if err != nil {
		return err
	}
	// embed clears the staged chunks as it swaps them in. Any other way out
	// would leave them behind for good, and the chunks embedded under the
	// run with them: a retry stages and embeds its own under a new run.
	defer w.clearStaging(ctx, logger, args.AccountID, runID, runnerRunID)
	deadline, resources := file.RunnerFor(account)
	sup := runSupervision(w.Store, args.AccountID, runnerRunID, "", "", w.superviseEvery, logger)
	res, cause := supervise(ctx, sup, w.Executor, executor.Spec{
		RunID: runnerRunID,
		Labels: map[string]string{
			"account": account.Key(), "repository": repo.name, "kind": jobs.QueueIndex,
		},
		Annotations: map[string]string{"river-job-id": strconv.FormatInt(job.ID, 10), "head-sha": commit},
		Job: runner.Spec{
			Version: runner.SpecVersion, Kind: runner.KindIndex, RunID: runnerRunID, CloneURL: client.CloneURL(owner, name),
			Head: commit, Base: base, Ignore: eff.Ignore,
		},
		Secrets:  runner.Secrets{GitToken: token},
		Deadline: deadline, Resources: resources,
	})
	if err := recordRun(ctx, w.Store, w.Metrics, account.Key(), args.AccountID, runnerRunID, jobs.QueueIndex, res); err != nil {
		return err
	}
	if res.Err != nil {
		reason := res.Err.Error()
		if errors.Is(cause, errHeartbeatLost) {
			reason = "runner heartbeat lost"
		}
		logger.Warn("index runner failed", "error", reason, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.IndexRun(account.Key(), mode, "failed", 0)
		// An error, so River tries again: most runner failures (a fetch
		// timeout, a node going away) do not repeat.
		return errors.Join(fmt.Errorf("worker: index runner failed: %s", reason), w.finish(ctx, args.AccountID, runID, "failed", 0, reason))
	}
	n, mode, err := w.embed(ctx, args, account, embedder, emb.Model, commit, runID, runnerRunID, active, settings, job.ID)
	if err != nil {
		logger.Error("index embedding failed", "error", err)
		w.Metrics.IndexRun(account.Key(), mode, "failed", 0)
		_ = w.finish(ctx, args.AccountID, runID, "failed", 0, err.Error())
		return err
	}
	logger.Info("index completed", "mode", mode, "chunks", n)
	w.Metrics.IndexRun(account.Key(), mode, "completed", n)
	if err := w.finish(ctx, args.AccountID, runID, "completed", n, ""); err != nil {
		return err
	}
	// A push while this ran was absorbed into this job: if the branch has
	// moved, index again at once, as a snooze that is not an attempt. A
	// forced rebuild leaves the new tip to the update job the push queued.
	if !args.Full {
		if tip, _, err := client.BranchTip(ctx, owner, name, repo.defaultBranch); err == nil && tip != commit {
			logger.Info("index again: the branch moved while it ran", "tip", short(tip))
			return river.JobSnooze(0)
		}
	}
	return nil
}

func (w *Index) loadRepo(ctx context.Context, args jobs.IndexArgs) (*indexRepo, error) {
	var r indexRepo
	var active *string
	err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT name, default_branch, enabled, active_index_run_id::text
			FROM repositories WHERE id = $1`, args.RepositoryID).
			Scan(&r.name, &r.defaultBranch, &r.enabled, &active)
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

func (w *Index) activeGeneration(ctx context.Context, accountID, runID string) (*activeGeneration, error) {
	if runID == "" {
		return nil, nil
	}
	g := &activeGeneration{id: runID}
	err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
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

func (w *Index) start(
	ctx context.Context, args jobs.IndexArgs, emb *configfile.Embedding, commit, base, mode string,
) (runID, runnerRunID string, err error) {
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		// Only the active generation keeps its chunks: any other run's were
		// left by a job that could not clear them, such as one killed
		// mid-build. A forced rebuild can run beside an update, so a run is
		// swept only once River would have given its job up for dead.
		if _, err := tx.Exec(ctx, `DELETE FROM index_chunks WHERE index_run_id IN (
			SELECT x.id FROM index_runs x JOIN repositories r ON r.id = x.repository_id
			WHERE r.id = $1 AND x.id IS DISTINCT FROM r.active_index_run_id AND x.created_at < now() - make_interval(secs => $2))`,
			args.RepositoryID, jobtimeout.RescueStuckJobsAfter.Seconds()); err != nil {
			return fmt.Errorf("worker: drop stray chunks: %w", err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO index_runs
			(account_id, repository_id, commit_sha, base_sha, embed_model, embed_dims, mode, status, trigger)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'running', $8) RETURNING id`,
			args.AccountID, args.RepositoryID, commit, base, emb.Model, emb.Dims, mode, args.Trigger).Scan(&runID); err != nil {
			return fmt.Errorf("worker: insert index run: %w", err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, index_run_id, kind) VALUES ($1, $2, 'index') RETURNING id`,
			args.AccountID, runID).Scan(&runnerRunID); err != nil {
			return fmt.Errorf("worker: insert runner run: %w", err)
		}
		return nil
	})
	return runID, runnerRunID, err
}

// clearStaging deletes a run's staged chunks, and the chunks it embedded
// unless they became the active generation, on a context of its own, so a
// job cut short still does, and logs a failure to logger.
func (w *Index) clearStaging(ctx context.Context, logger *slog.Logger, accountID, runID, runnerRunID string) {
	ctx, cancel := detach(ctx)
	defer cancel()
	err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM index_staging WHERE runner_run_id = $1`, runnerRunID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM index_chunks WHERE index_run_id = $1
			AND NOT EXISTS (SELECT 1 FROM repositories WHERE active_index_run_id = $1)`, runID)
		return err
	})
	if err != nil {
		logger.Warn("staged chunks not cleared", "run", runnerRunID, "error", err)
	}
}

func (w *Index) finish(ctx context.Context, accountID, runID, status string, chunks int, errText string) error {
	return w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
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
}

type stagedChunk struct {
	id                                        int64
	path, language, symbol, kind, scope, text string
	startLine, endLine                        int
}

// embed turns the staged chunks into index_chunks rows under the run, a
// batch at a time with no transaction open while the embedder works, then
// swaps them in with one short transaction, so a review never sees a
// half-built index: a full build makes its run the active generation, and
// an incremental step moves its chunks into the active generation, in place
// of the changed paths' chunks.
func (w *Index) embed(
	ctx context.Context, args jobs.IndexArgs, account *configfile.Account, embedder model.Embedder, embedModel string,
	commit, runID, runnerRunID string, active *activeGeneration, settings configfile.Settings, jobID int64,
) (int, string, error) {
	var pack indexPack
	err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mode, base_sha, changed_paths FROM index_packs WHERE runner_run_id = $1`, runnerRunID).
			Scan(&pack.mode, &pack.base, &pack.changedPaths)
	})
	if err != nil {
		return 0, "", fmt.Errorf("worker: read index pack: %w", err)
	}
	// The runner may have fallen back to a full build; the run row says
	// what actually happened.
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE index_runs SET mode = $2, base_sha = $3 WHERE id = $1`, runID, pack.mode, pack.base)
		return err
	})
	if err != nil {
		return 0, pack.mode, fmt.Errorf("worker: record index mode: %w", err)
	}
	var total int
	var tokens int64
	err = w.withLease(ctx, account, "embed:"+embedModel, settings.Limits.Concurrency, jobID, func(ctx context.Context) error {
		var last int64
		for {
			var batch []stagedChunk
			err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
				var err error
				batch, err = readStaged(ctx, tx, runnerRunID, last, cmp.Or(w.batch, embedBatch))
				return err
			})
			if err != nil || len(batch) == 0 {
				return err
			}
			texts := make([]string, len(batch))
			for i, c := range batch {
				texts[i] = embedText(c)
			}
			vectors, used, err := embedder.Embed(ctx, texts)
			if err != nil {
				w.Metrics.ModelCall(account.Key(), embedModel, roleEmbedding, "error", 0, 0, 0, 0)
				return err
			}
			w.Metrics.ModelCall(account.Key(), embedModel, roleEmbedding, "ok", used, 0, 0, 0)
			tokens += used
			err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
				return insertChunks(ctx, tx, args.AccountID, args.RepositoryID, runID, batch, vectors)
			})
			if err != nil {
				return err
			}
			total += len(batch)
			last = batch[len(batch)-1].id
		}
	})
	if err != nil {
		return total, pack.mode, err
	}
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		if pack.mode == modeIncremental && active != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM index_chunks WHERE index_run_id = $1 AND path = ANY($2)`,
				active.id, pack.changedPaths); err != nil {
				return fmt.Errorf("worker: drop stale chunks: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE index_chunks SET index_run_id = $2 WHERE index_run_id = $1`, runID, active.id); err != nil {
				return fmt.Errorf("worker: move chunks into the generation: %w", err)
			}
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
		if _, err := tx.Exec(ctx, `INSERT INTO usage (account_id, repository_id, role, model, input_tokens) VALUES ($1, $2, 'embedding', $3, $4)`,
			args.AccountID, args.RepositoryID, embedModel, tokens); err != nil {
			return fmt.Errorf("worker: record embedding usage: %w", err)
		}
		return nil
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

func insertChunks(ctx context.Context, tx pgx.Tx, accountID, repositoryID, runID string, batch []stagedChunk, vectors [][]float32) error {
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
			(account_id, repository_id, index_run_id, path, start_line, end_line, language, symbol, kind, scope, text, embedding)
		SELECT $1, $2, $3, c.path, c.start_line, c.end_line, c.language, c.symbol, c.kind, c.scope, c.text, c.embedding::halfvec
		FROM unnest($4::text[], $5::int[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::text[])
			AS c (path, start_line, end_line, language, symbol, kind, scope, text, embedding)`,
		accountID, repositoryID, runID, paths, starts, ends, langs, symbols, kinds, scopes, texts, embeddings)
	if err != nil {
		return fmt.Errorf("worker: insert chunks: %w", err)
	}
	return nil
}

// recordRun writes what the executor learned about a runner Job and counts
// it.
func recordRun(
	ctx context.Context, st *store.Store, m *metrics.Metrics, account, accountID, runID, kind string, res executor.Result,
) error {
	outcome := runOutcome(res)
	var took time.Duration
	if !res.StartedAt.IsZero() {
		took = time.Since(res.StartedAt)
	}
	m.RunnerRun(account, kind, outcome, took)
	return st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
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
