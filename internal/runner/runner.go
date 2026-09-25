// Package runner is what a runner pod does: fetch the head and merge-base
// (and the last reviewed head when there is one), diff them, compute the
// patch id, and write a context pack under its own run id. It works from
// one versioned job document (Spec); its credentials, a git token for one
// repository and for an agentic review a model key, arrive apart from it
// (Secrets). Its database role can only touch its own run.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
)

// Run executes one run and reports success or failure in the run row,
// stamping a heartbeat while it works. The store must be opened with the
// runner role's DSN.
func Run(ctx context.Context, st *store.Store, spec Spec, secrets Secrets, logger *slog.Logger) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	hctx, stop := context.WithCancel(ctx)
	beating := make(chan struct{})
	go func() {
		defer close(beating)
		heartbeat(hctx, HeartbeatInterval, func(ctx context.Context) error { return beat(ctx, st, spec.RunID) }, logger)
	}()
	defer func() {
		stop()
		<-beating
	}()
	if spec.Kind == KindIndex {
		return runIndex(ctx, st, spec, secrets, logger)
	}
	return runReview(ctx, st, spec, secrets, logger)
}

func runReview(ctx context.Context, st *store.Store, p Spec, secrets Secrets, logger *slog.Logger) error {
	if err := setPhase(ctx, st, p.RunID, "fetching"); err != nil {
		return err
	}
	res, err := gitfetch.Run(ctx, gitfetch.Fetch{
		CloneURL: p.CloneURL, Token: secrets.GitToken, Head: p.Head, Base: p.Base, Prior: p.PriorHead,
	})
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	defer func() { _ = res.Close() }()
	logger.Info("fetched", "head", p.Head[:7], "base", p.Base[:7], "changed_paths", len(res.Changed), "diff_bytes", len(res.Diff))

	if err := setPhase(ctx, st, p.RunID, "parsing"); err != nil {
		return err
	}
	baseTree, err := res.Base.Tree()
	if err != nil {
		err = fmt.Errorf("runner: base tree: %w", err)
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	repoFiles, repoNotes, ignore, err := repoConfig(baseTree, p.Ignore, p.RepoFiles)
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	// A nil prior head tells the worker the delta is unknown, not empty.
	var priorHead *string
	deltaPaths := []string{}
	if res.Prior != nil {
		priorHead, deltaPaths = &p.PriorHead, notIgnored(res.DeltaChanged, ignore)
		logger.Info("fetched prior head", "prior", p.PriorHead[:7], "delta_paths", len(deltaPaths), "delta_bytes", len(res.DeltaDiff))
	} else if p.PriorHead != "" {
		// Best effort: the review goes on in full. A force-push is the
		// expected cause; the error tells it apart from auth or network.
		logger.Warn("prior head not fetched", "prior", p.PriorHead[:7], "error", res.PriorErr)
	}
	chunks, stats, err := stages(ctx, res, ignore)
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	logger.Info("context built", "overlay", stats.Overlay, "definitions", stats.Definitions, "callers", stats.Callers,
		"identifiers", stats.Identifiers, "files_scanned", stats.FilesScanned, "files_parsed", stats.FilesParsed,
		"scan_truncated", stats.ScanTruncated, "elapsed", stats.Elapsed.Round(time.Millisecond))
	stagesJSON, err := json.Marshal(chunks)
	if err != nil {
		return fmt.Errorf("runner: encode context: %w", err)
	}
	filesJSON, err := json.Marshal(repoFiles)
	if err != nil {
		return fmt.Errorf("runner: encode repository files: %w", err)
	}
	if repoNotes == nil {
		repoNotes = []string{}
	}

	if err := setPhase(ctx, st, p.RunID, "writing"); err != nil {
		return err
	}
	// An agentic run is not done until its agent has run too.
	next := "done"
	if p.Mode == ModeAgentic {
		next = "reviewing"
	}
	err = st.WithRunnerJob(ctx, p.RunID, func(tx pgx.Tx) error {
		// tenant_id is copied from the run row: the runner never receives it
		// and cannot invent one, and the policy only opens its own run.
		_, err := tx.Exec(ctx, `
			INSERT INTO context_packs (runner_run_id, tenant_id, head_sha, base_sha, patch_id, diff, changed_paths, stages, repo_files, repo_notes,
				prior_head_sha, delta_diff, delta_paths)
			SELECT id, tenant_id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12 FROM runner_runs WHERE id = $1`,
			p.RunID, p.Head, p.Base, res.PatchID, res.Diff, res.Changed, stagesJSON, filesJSON, repoNotes,
			priorHead, res.DeltaDiff, deltaPaths)
		if err != nil {
			return fmt.Errorf("runner: write context pack: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE runner_runs SET phase = $2 WHERE id = $1`, p.RunID, next)
		return err
	})
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	logger.Info("context pack written", "run", p.RunID, "patch_id", res.PatchID[:12])
	if p.Mode != ModeAgentic {
		return nil
	}
	headTree, err := res.Head.Tree()
	if err != nil {
		err = fmt.Errorf("runner: head tree: %w", err)
	} else {
		scope, _ := review.DecideScope(p.PriorHead != "", priorHead != nil, len(deltaPaths), p.Prompt.MaxDeltaFiles)
		err = runAgentic(ctx, st, p, secrets, headTree, repoFiles, packView{
			Diff: res.Diff, Changed: res.Changed, Context: chunks, DeltaDiff: res.DeltaDiff, Scope: scope,
		}, ignore, res.PatchID, logger)
	}
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	return nil
}

// notIgnored returns the paths no ignore glob matches, never nil.
func notIgnored(paths, ignore []string) []string {
	out := []string{}
	for _, p := range paths {
		if !slices.ContainsFunc(ignore, func(g string) bool { ok, _ := doublestar.Match(g, p); return ok }) {
			out = append(out, p)
		}
	}
	return out
}

// stages runs context stages 1 to 3 over the fetched trees. The chunk list
// is never nil so the column holds a JSON array even for an empty pack.
func stages(ctx context.Context, res *gitfetch.Result, ignore []string) ([]contextpack.Chunk, contextpack.Stats, error) {
	headTree, err := res.Head.Tree()
	if err != nil {
		return nil, contextpack.Stats{}, fmt.Errorf("runner: head tree: %w", err)
	}
	baseTree, err := res.Base.Tree()
	if err != nil {
		return nil, contextpack.Stats{}, fmt.Errorf("runner: base tree: %w", err)
	}
	chunks, stats, err := contextpack.Build(ctx, contextpack.Input{
		Head: headTree, Base: baseTree, Diff: res.Diff, Changed: res.Changed, Ignore: ignore,
	}, contextpack.DefaultOptions)
	if err != nil {
		return nil, stats, fmt.Errorf("runner: context stages: %w", err)
	}
	if chunks == nil {
		chunks = []contextpack.Chunk{}
	}
	return chunks, stats, nil
}

// heartbeat calls beat now and then every interval until ctx ends. A failed
// beat is logged and retried on the next tick: one lost write must not end
// a run the worker would otherwise see recover.
func heartbeat(ctx context.Context, interval time.Duration, beat func(context.Context) error, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := beat(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func beat(ctx context.Context, st *store.Store, runID string) error {
	return st.WithRunnerJob(ctx, runID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runner_runs SET heartbeat_at = now() WHERE id = $1`, runID); err != nil {
			return fmt.Errorf("runner: heartbeat: %w", err)
		}
		return nil
	})
}

func setPhase(ctx context.Context, st *store.Store, runID, phase string) error {
	return st.WithRunnerJob(ctx, runID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = $2 WHERE id = $1`, runID, phase)
		if err != nil {
			return fmt.Errorf("runner: set phase: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("runner: run %s is not visible to this role", runID)
		}
		return nil
	})
}

func fail(ctx context.Context, st *store.Store, runID string, cause error) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return st.WithRunnerJob(fctx, runID, func(tx pgx.Tx) error {
		_, err := tx.Exec(fctx, `UPDATE runner_runs SET phase = 'failed', error = left($2, 2000) WHERE id = $1`, runID, cause.Error())
		return err
	})
}
