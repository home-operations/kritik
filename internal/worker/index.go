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

	"github.com/home-operations/kritik/internal/adapter"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
)

// embedBatch is how many staged chunks are embedded and inserted at once.
const embedBatch = 128

// Index works the index queue: one job builds or advances a repository's
// embedding index to a commit. The runner Job chunks the tree; this worker
// embeds the chunks and swaps or advances the active generation.
type Index struct {
	river.WorkerDefaults[jobs.IndexArgs]
	Base
	Executor executor.Executor
	// Embedders resolves the instance's embedder; a generation built with
	// another model or dimension is rebuilt in full.
	Embedders *adapter.Embedders

	// superviseEvery overrides superviseInterval.
	superviseEvery time.Duration
	// batch overrides embedBatch.
	batch int
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
	var repo store.IndexRepo
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		var err error
		repo, err = store.FindIndexRepo(ctx, tx, args.RepositoryID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return river.JobCancel(fmt.Errorf("worker: repository %s is unknown", args.RepositoryID))
	}
	if err != nil {
		return err
	}
	logger := w.Logger.With("account", account.Key(), "repository", repo.Name, "trigger", args.Trigger)
	settings := file.Settings(account, repo.Name)
	if !repo.Enabled || !file.Runs(account, repo.Name, repo.Traits) {
		logger.Info("index skipped, repository disabled")
		return nil
	}
	client, err := w.client(ctx, file, account, repo.Name)
	if err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo.Name, "/")
	// The job indexes the tip, not the commit of the push that queued it:
	// pushes while it waited were absorbed into it.
	commit, branch, err := client.BranchTip(ctx, owner, name, repo.DefaultBranch)
	if err != nil {
		return err
	}
	if repo.DefaultBranch == "" {
		// Best effort: the run has the branch either way, and the next one
		// looks it up again until this lands.
		err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
			return store.RecordDefaultBranch(ctx, tx, args.RepositoryID, branch)
		})
		if err != nil {
			logger.Warn("default branch not recorded", "branch", branch, "error", err)
		}
	}
	logger = logger.With("commit", review.ShortSHA(commit))

	active, err := w.activeGeneration(ctx, args.AccountID, repo.ActiveRun)
	if err != nil {
		return err
	}
	mode, base := store.IndexModeFull, ""
	if !args.Full && active != nil && active.Model == emb.Model && active.Dims == emb.Dims {
		if active.Commit == commit {
			logger.Info("index already at this commit")
			return nil
		}
		mode, base = store.IndexModeIncremental, active.Commit
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
	token, err := client.GitToken(ctx, name)
	if err != nil {
		return err
	}
	var runID, runnerRunID string
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		var err error
		runID, runnerRunID, err = store.StartIndexRun(ctx, tx, store.NewIndexRun{
			AccountID: args.AccountID, RepositoryID: args.RepositoryID, Commit: commit, Base: base, Mode: mode, Trigger: args.Trigger,
			Embedding: *emb, StrayAfter: jobtimeout.RescueStuckJobsAfter,
		})
		return err
	})
	if err != nil {
		return err
	}
	// embed clears the staged chunks as it swaps them in. Any other way out
	// would leave them behind for good, and the chunks embedded under the
	// run with them: a retry stages and embeds its own under a new run.
	defer w.clearStaging(ctx, logger, args.AccountID, runID, runnerRunID)
	deadline, resources := file.RunnerFor()
	sup := runSupervision(w.Store, args.AccountID, runnerRunID, "", "", w.superviseEvery, logger)
	res, cause := supervise(ctx, sup, w.Executor, executor.Spec{
		Labels: map[string]string{
			"account": account.Key(), "repository": repo.Name, "kind": jobs.QueueIndex,
		},
		Annotations: map[string]string{"river-job-id": strconv.FormatInt(job.ID, 10), "head-sha": commit},
		Job: runner.Spec{
			Version: runner.SpecVersion, Kind: runner.KindIndex, RunID: runnerRunID, CloneURL: client.CloneURL(owner, name),
			Head: commit, Base: base, Ignore: eff.Ignore,
		},
		Secrets:  runner.Secrets{GitToken: token},
		Deadline: deadline, Resources: resources,
	})
	// The run's record and a failed run's end must land even once River's
	// timeout has ended ctx, the likeliest reason the run failed.
	dctx, cancel := detach(ctx)
	defer cancel()
	if err := recordRun(dctx, w.Store, w.Metrics, account.Key(), args.AccountID, runnerRunID, jobs.QueueIndex, res); err != nil {
		return err
	}
	if res.Err != nil {
		reason := res.Err.Error()
		if errors.Is(cause, errHeartbeatLost) {
			reason = "runner heartbeat lost"
		}
		logger.Warn("index runner failed", "error", reason, "job", res.JobName, "reason", res.TerminationReason)
		w.Metrics.IndexRun(account.Key(), mode, string(store.IndexFailed), 0)
		// An error, so River tries again: most runner failures (a fetch
		// timeout, a node going away) do not repeat.
		return errors.Join(fmt.Errorf("worker: index runner failed: %s", reason),
			w.finish(dctx, args.AccountID, runID, store.IndexFailed, 0, reason))
	}
	n, mode, err := w.embed(ctx, args, account, embedder, emb.Model, commit, runID, runnerRunID, active, settings, job.ID)
	if err != nil {
		logger.Error("index embedding failed", "error", err)
		w.Metrics.IndexRun(account.Key(), mode, string(store.IndexFailed), 0)
		fctx, fcancel := detach(ctx)
		defer fcancel()
		return errors.Join(err, w.finish(fctx, args.AccountID, runID, store.IndexFailed, 0, err.Error()))
	}
	logger.Info("index completed", "mode", mode, "chunks", n)
	w.Metrics.IndexRun(account.Key(), mode, string(store.IndexCompleted), n)
	if err := w.finish(ctx, args.AccountID, runID, store.IndexCompleted, n, ""); err != nil {
		return err
	}
	// A push while this ran was absorbed into this job: if the branch has
	// moved, index again at once, as a snooze that is not an attempt. A
	// forced rebuild leaves the new tip to the update job the push queued.
	if !args.Full {
		if tip, _, err := client.BranchTip(ctx, owner, name, repo.DefaultBranch); err == nil && tip != commit {
			logger.Info("index again: the branch moved while it ran", "tip", review.ShortSHA(tip))
			return river.JobSnooze(0)
		}
	}
	return nil
}

// activeGeneration reads the repository's active generation, nil when it
// has none or the generation is not completed.
func (w *Index) activeGeneration(ctx context.Context, accountID, runID string) (*store.Generation, error) {
	if runID == "" {
		return nil, nil
	}
	var g store.Generation
	err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		g, err = store.FindGeneration(ctx, tx, runID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// clearStaging deletes a run's staged chunks, and the chunks it embedded
// unless they became the active generation, on a context of its own, so a
// job cut short still does, and logs a failure to logger.
func (w *Index) clearStaging(ctx context.Context, logger *slog.Logger, accountID, runID, runnerRunID string) {
	ctx, cancel := detach(ctx)
	defer cancel()
	err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return store.ClearIndexStaging(ctx, tx, runID, runnerRunID)
	})
	if err != nil {
		logger.Warn("staged chunks not cleared", "run", runnerRunID, "error", err)
	}
}

func (w *Index) finish(ctx context.Context, accountID, runID string, status store.IndexRunStatus, chunks int, errText string) error {
	return w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return store.FinishIndexRun(ctx, tx, runID, status, chunks, errText)
	})
}

// embed turns the staged chunks into index_chunks rows under the run, a
// batch at a time with no transaction open while the embedder works, then
// swaps them in with one short transaction, so a review never sees a
// half-built index: a full build makes its run the active generation, and
// an incremental step moves its chunks into the active generation, in place
// of the changed paths' chunks.
func (w *Index) embed(
	ctx context.Context, args jobs.IndexArgs, account *configfile.Account, embedder model.Embedder, embedModel string,
	commit, runID, runnerRunID string, active *store.Generation, settings configfile.Settings, jobID int64,
) (int, string, error) {
	var pack store.IndexPack
	err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		var err error
		pack, err = store.ReadIndexPack(ctx, tx, runID, runnerRunID)
		return err
	})
	if err != nil {
		return 0, pack.Mode, err
	}
	var total int
	var tokens int64
	err = w.withLease(ctx, account, "embed:"+embedModel, settings.Limits.Concurrency, jobID, func(ctx context.Context) error {
		var last int64
		for {
			var batch []store.StagedChunk
			err := w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
				var err error
				batch, err = store.ReadStagedChunks(ctx, tx, runnerRunID, last, cmp.Or(w.batch, embedBatch))
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
				w.Metrics.ModelCall(account.Key(), embedModel, store.RoleEmbedding, "error", 0, 0, 0, 0)
				return err
			}
			w.Metrics.ModelCall(account.Key(), embedModel, store.RoleEmbedding, "ok", used, 0, 0, 0)
			tokens += used
			err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
				return store.InsertIndexChunks(ctx, tx, args.AccountID, args.RepositoryID, runID, batch, vectors)
			})
			if err != nil {
				return err
			}
			total += len(batch)
			last = batch[len(batch)-1].ID
		}
	})
	if err != nil {
		return total, pack.Mode, err
	}
	err = w.Store.WithAccount(ctx, args.AccountID, func(tx pgx.Tx) error {
		if pack.Mode == store.IndexModeIncremental && active != nil {
			if err := store.AdvanceGeneration(ctx, tx, active.ID, runID, commit, pack.ChangedPaths); err != nil {
				return err
			}
		} else {
			previous := ""
			if active != nil {
				previous = active.ID
			}
			if err := store.ActivateGeneration(ctx, tx, args.RepositoryID, runID, previous); err != nil {
				return err
			}
		}
		if err := store.ClearIndexStaging(ctx, tx, runID, runnerRunID); err != nil {
			return err
		}
		return store.InsertUsage(ctx, tx, store.Usage{
			AccountID: args.AccountID, RepositoryID: args.RepositoryID, Role: store.RoleEmbedding, Model: embedModel, Input: tokens,
		})
	})
	return total, pack.Mode, err
}

// embedText is what the embedder sees: the path and symbol give the
// vector a little of the file's identity, the way a reader would know
// where a snippet came from.
func embedText(c store.StagedChunk) string {
	head := c.Path
	if c.Symbol != "" {
		head += " " + c.Kind + " " + c.Symbol
	}
	return head + "\n" + c.Text
}

// recordRun writes what the executor learned about a runner Job and counts
// it.
func recordRun(
	ctx context.Context, st *store.Store, m *metrics.Metrics, account, accountID, runID, kind string, res executor.Result,
) error {
	var took time.Duration
	if !res.StartedAt.IsZero() {
		took = time.Since(res.StartedAt)
	}
	m.RunnerRun(account, kind, runOutcome(res), took)
	return st.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		return store.RecordRunnerRun(ctx, tx, runID, store.RunnerResult{
			JobName: res.JobName, PodName: res.PodName, NodeName: res.NodeName, ScheduledAt: res.ScheduledAt, StartedAt: res.StartedAt,
			ExitCode: res.ExitCode, TerminationReason: res.TerminationReason, DeadlineExceeded: res.DeadlineExceeded, LogTail: res.LogTail,
			Error: errText(res.Err),
		})
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
