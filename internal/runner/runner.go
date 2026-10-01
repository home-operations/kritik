// Package runner is what a runner pod does: fetch the head and merge-base
// (and the last reviewed head when there is one), diff them, compute the
// patch id, decide what the review builds on and whether it is skipped,
// write a context pack under its own run id, and run the review's agent
// over it. It works from
// one versioned job document (Spec); its credentials, a git token for one
// repository and for a review a token for the worker's model gateway,
// arrive apart from it (Secrets). Its database role can only touch
// its own run.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/agent"
	"github.com/home-operations/kritika/internal/chunk"
	"github.com/home-operations/kritika/internal/contextpack"
	"github.com/home-operations/kritika/internal/gitfetch"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
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
	run := runReview
	if spec.Kind == KindIndex {
		run = runIndex
	}
	// The first thing either kind does is fetch through the gateway, which
	// a runner started with the service may reach before its Service
	// does.
	err := waitForGateway(ctx, logger)
	if err == nil {
		err = run(ctx, st, spec, secrets, logger)
	}
	if err != nil {
		// The worker still sees the Job fail; the row only loses why.
		if ferr := fail(ctx, st, spec.RunID, secrets, err); ferr != nil {
			logger.Warn("run failure not recorded", "error", ferr)
		}
	}
	return err
}

func runReview(ctx context.Context, st *store.Store, p Spec, secrets Secrets, logger *slog.Logger) error {
	if err := setPhase(ctx, st, p.RunID, "fetching"); err != nil {
		return err
	}
	res, err := gitfetch.Run(ctx, gitfetch.Fetch{
		CloneURL: p.CloneURL, Token: secrets.GitToken, Head: p.Head, Base: p.Base, Prior: p.PriorHead,
	})
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	logger.Info("fetched", "head", p.Head[:7], "base", p.Base[:7], "changed_paths", len(res.Changed), "diff_bytes", len(res.Diff))

	if err := setPhase(ctx, st, p.RunID, "parsing"); err != nil {
		return err
	}
	headTree, err := res.Head.Tree()
	if err != nil {
		return fmt.Errorf("runner: head tree: %w", err)
	}
	baseTree, err := res.Base.Tree()
	if err != nil {
		return fmt.Errorf("runner: base tree: %w", err)
	}
	var agentChanged []string
	if p.AgentFiles {
		agentChanged = res.Changed
	}
	files, notes, err := repoFiles(baseTree, p.RepoFiles, agentChanged)
	if err != nil {
		return err
	}
	ignore := p.Ignore
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
	chunks, stats, err := stages(ctx, headTree, baseTree, res, ignore)
	if err != nil {
		return err
	}
	logger.Info("context built", "overlay", stats.Overlay, "definitions", stats.Definitions, "callers", stats.Callers,
		"identifiers", stats.Identifiers, "files_scanned", stats.FilesScanned, "files_parsed", stats.FilesParsed,
		"scan_truncated", stats.ScanTruncated, "elapsed", stats.Elapsed.Round(time.Millisecond))

	// Everything the worker reads back is decided here, before the pack is
	// written: whether the review is skipped, what it builds on, and what
	// the prompt was given. A skipped review spends nothing on a model.
	scope, scopeReason := review.DecideScope(p.PriorHead != "", p.PriorHead == p.Head, priorHead != nil,
		len(deltaPaths), p.Prompt.MaxDeltaFiles)
	skip := agentSkip(p, res.Changed, res.PatchID, res.Diff)
	in := newPromptInputs(p, files, res.Changed)
	notes = append(notes, in.notes...)
	var tools agentTools
	var prompt agentPrompt
	if skip == "" {
		// Stage 4, best effort: an instance without an embedder, a
		// repository without a completed index, or a refused or failed
		// request leaves it out and the review goes on.
		if queries := hunkQueries(res.Diff); len(queries) > 0 {
			out, err := similarCode(ctx, p.Model.GatewayURL, secrets.GatewayToken,
				contextpack.SimilarRequest{Queries: queries, Exclude: res.Changed})
			switch {
			case err != nil:
				logger.Warn("similar-code retrieval skipped", "error", err)
			case out.Indexed:
				chunks = append(chunks, out.Chunks...)
				tools.search = &searchTool{
					gatewayURL: p.Model.GatewayURL, token: secrets.GatewayToken, exclude: res.Changed, maxBytes: p.Agent.limits().MaxToolOutputBytes,
				}
			}
		}
		var cleanup func()
		tools.run, cleanup = commandTool(ctx, p, agent.NewTree(headTree, ignore), secrets.GitToken, p.Agent.limits().MaxToolOutputBytes, logger)
		defer cleanup()
		prompt = newAgentPrompt(p, in, packView{
			Diff: res.Diff, Changed: res.Changed, Context: chunks, DeltaDiff: res.DeltaDiff, Scope: scope,
		}, tools.commands(), tools.search != nil)
		notes = append(notes, prompt.notes()...)
	}
	stagesJSON, err := json.Marshal(chunks)
	if err != nil {
		return fmt.Errorf("runner: encode context: %w", err)
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("runner: encode repository files: %w", err)
	}
	if notes == nil {
		notes = []string{}
	}

	if err := setPhase(ctx, st, p.RunID, "writing"); err != nil {
		return err
	}
	err = st.WithRunnerJob(ctx, p.RunID, func(tx pgx.Tx) error {
		// account_id is copied from the run row: the runner never receives it
		// and cannot invent one, and the policy only opens its own run.
		_, err := tx.Exec(ctx, `
			INSERT INTO context_packs (runner_run_id, account_id, head_sha, base_sha, patch_id, diff, changed_paths, stages, repo_files, repo_notes,
				prior_head_sha, delta_diff, delta_paths, scope, scope_reason, skip_reason, rule_ids)
			SELECT id, account_id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16 FROM runner_runs WHERE id = $1`,
			p.RunID, p.Head, p.Base, res.PatchID, res.Diff, res.Changed, stagesJSON, filesJSON, notes,
			priorHead, res.DeltaDiff, deltaPaths, string(scope), scopeReason, skip, in.ruleIDs())
		if err != nil {
			return fmt.Errorf("runner: write context pack: %w", err)
		}
		if skip != "" {
			return setPhaseTx(ctx, tx, p.RunID, "done")
		}
		// A review is not done until its agent has run too.
		return setPhaseTx(ctx, tx, p.RunID, "reviewing")
	})
	if err != nil {
		return err
	}
	logger.Info("context pack written", "run", p.RunID, "patch_id", res.PatchID[:12], "scope", scope, "skip", skip)
	if skip != "" {
		logger.Info("agent not run", "reason", skip)
		return nil
	}
	return runAgentic(ctx, st, p, secrets, headTree, ignore, tools, prompt, scope, logger)
}

// notIgnored returns the paths no ignore glob matches, never nil.
func notIgnored(paths, ignore []string) []string {
	out := []string{}
	for _, p := range paths {
		if !chunk.Ignored(ignore, p) {
			out = append(out, p)
		}
	}
	return out
}

// stages runs context stages 1 to 3 over the fetched trees. The chunk list
// is never nil so the column holds a JSON array even for an empty pack.
func stages(
	ctx context.Context, headTree, baseTree *object.Tree, res *gitfetch.Result, ignore []string,
) ([]contextpack.Chunk, contextpack.Stats, error) {
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
	return st.WithRunnerJob(ctx, runID, func(tx pgx.Tx) error { return setPhaseTx(ctx, tx, runID, phase) })
}

// setPhaseTx is setPhase in tx, alongside what the run wrote there.
func setPhaseTx(ctx context.Context, tx pgx.Tx, runID, phase string) error {
	tag, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = $2 WHERE id = $1`, runID, phase)
	if err != nil {
		return fmt.Errorf("runner: set phase: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("runner: run %s is not visible to this role", runID)
	}
	return nil
}

// fail records cause as the run's error, with its secrets masked: a git or
// provider error may carry a credential.
func fail(ctx context.Context, st *store.Store, runID string, secrets Secrets, cause error) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := st.WithRunnerJob(fctx, runID, func(tx pgx.Tx) error {
		_, err := tx.Exec(fctx, `UPDATE runner_runs SET phase = 'failed', error = left($2, 2000) WHERE id = $1`,
			runID, secrets.Mask(cause.Error()))
		return err
	})
	if err != nil {
		return fmt.Errorf("runner: record failure: %w", err)
	}
	return nil
}
