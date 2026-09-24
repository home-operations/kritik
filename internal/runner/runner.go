// Package runner is what a runner pod does: fetch the two commits, diff
// them, compute the patch id, and write a context pack under its own run
// id. It holds a git token for one repository and a database role that can
// only touch its own run. It never sees a model key.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/store"
)

// Kinds of run.
const (
	KindReview = "review"
	KindIndex  = "index"
)

// Params is everything a run needs, passed to the pod as environment.
type Params struct {
	// Kind is review or index; empty means review.
	Kind string
	// RunID is the runner_runs row this pod owns.
	RunID string
	// CloneURL and Token fetch the repository.
	CloneURL string
	Token    string
	// Head and Base are the commits to diff; Base is the merge-base.
	Head, Base string
	// Ignore globs are skipped by the context stages.
	Ignore []string
}

// Run executes one run and reports success or failure in the run row. The
// store must be opened with the runner role's DSN.
func Run(ctx context.Context, st *store.Store, p Params, logger *slog.Logger) error {
	if p.Kind == KindIndex {
		return runIndex(ctx, st, p, logger)
	}
	if err := setPhase(ctx, st, p.RunID, "fetching"); err != nil {
		return err
	}
	res, err := gitfetch.Run(ctx, gitfetch.Fetch{CloneURL: p.CloneURL, Token: p.Token, Head: p.Head, Base: p.Base})
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	defer func() { _ = res.Close() }()
	logger.Info("fetched", "head", p.Head[:7], "base", p.Base[:7], "changed_paths", len(res.Changed), "diff_bytes", len(res.Diff))

	if err := setPhase(ctx, st, p.RunID, "parsing"); err != nil {
		return err
	}
	chunks, stats, err := stages(ctx, res, p.Ignore)
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

	if err := setPhase(ctx, st, p.RunID, "writing"); err != nil {
		return err
	}
	err = st.WithRunnerJob(ctx, p.RunID, func(tx pgx.Tx) error {
		// tenant_id is copied from the run row: the runner never receives it
		// and cannot invent one, and the policy only opens its own run.
		_, err := tx.Exec(ctx, `
			INSERT INTO context_packs (runner_run_id, tenant_id, head_sha, base_sha, patch_id, diff, changed_paths, stages)
			SELECT id, tenant_id, $2, $3, $4, $5, $6, $7 FROM runner_runs WHERE id = $1`,
			p.RunID, p.Head, p.Base, res.PatchID, res.Diff, res.Changed, stagesJSON)
		if err != nil {
			return fmt.Errorf("runner: write context pack: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE runner_runs SET phase = 'done' WHERE id = $1`, p.RunID)
		return err
	})
	if err != nil {
		_ = fail(ctx, st, p.RunID, err)
		return err
	}
	logger.Info("context pack written", "run", p.RunID, "patch_id", res.PatchID[:12])
	return nil
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
