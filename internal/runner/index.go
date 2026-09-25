package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/gitfetch"
	"github.com/home-operations/kritik/internal/indexer"
	"github.com/home-operations/kritik/internal/store"
)

// stagingBatch is how many staged chunks one INSERT carries.
const stagingBatch = 500

// runIndex fetches the commit to index and stages its chunks. With Base
// set it tries an incremental step from that commit first; if the base
// cannot be fetched (unreachable after a force push, or older than the
// server will serve by SHA) it falls back to a full build and says so in
// the pack, so the worker never has to retry the Job.
func runIndex(ctx context.Context, st *store.Store, p Spec, secrets Secrets, logger *slog.Logger) error {
	if err := setPhase(ctx, st, p.RunID, "fetching"); err != nil {
		return err
	}
	res, mode, err := fetchForIndex(ctx, p, secrets.GitToken, logger)
	if err != nil {
		_ = fail(ctx, st, p.RunID, secrets, err)
		return err
	}
	defer func() { _ = res.Close() }()

	if err := setPhase(ctx, st, p.RunID, "parsing"); err != nil {
		return err
	}
	headTree, err := res.Head.Tree()
	if err != nil {
		return fmt.Errorf("runner: head tree: %w", err)
	}
	var baseTree *object.Tree
	if mode == "incremental" {
		if baseTree, err = res.Base.Tree(); err != nil {
			return fmt.Errorf("runner: base tree: %w", err)
		}
	}
	chunks, changed, stats, err := indexer.Build(ctx, headTree, baseTree, p.Ignore, indexer.DefaultOptions)
	if err != nil {
		_ = fail(ctx, st, p.RunID, secrets, err)
		return err
	}
	logger.Info("chunked", "mode", mode, "chunks", len(chunks), "changed_paths", len(changed), "files", stats.Files,
		"parsed", stats.Parsed, "skipped", stats.Skipped, "truncated", stats.Truncated, "elapsed", stats.Elapsed.Round(time.Millisecond))

	if err := setPhase(ctx, st, p.RunID, "writing"); err != nil {
		return err
	}
	if changed == nil {
		changed = []string{}
	}
	err = st.WithRunnerJob(ctx, p.RunID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO index_packs (runner_run_id, tenant_id, commit_sha, base_sha, mode, changed_paths, chunk_count)
			SELECT id, tenant_id, $2, $3, $4, $5, $6 FROM runner_runs WHERE id = $1`,
			p.RunID, p.Head, baseFor(mode, p.Base), mode, changed, len(chunks)); err != nil {
			return fmt.Errorf("runner: write index pack: %w", err)
		}
		for start := 0; start < len(chunks); start += stagingBatch {
			end := min(start+stagingBatch, len(chunks))
			if err := stage(ctx, tx, p.RunID, chunks[start:end]); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = 'done' WHERE id = $1`, p.RunID)
		return err
	})
	if err != nil {
		_ = fail(ctx, st, p.RunID, secrets, err)
		return err
	}
	logger.Info("index pack written", "run", p.RunID, "chunks", len(chunks))
	return nil
}

func fetchForIndex(ctx context.Context, p Spec, token string, logger *slog.Logger) (*gitfetch.Result, string, error) {
	if p.Base != "" {
		res, err := gitfetch.Run(ctx, gitfetch.Fetch{CloneURL: p.CloneURL, Token: token, Head: p.Head, Base: p.Base})
		if err == nil {
			return res, "incremental", nil
		}
		if ctx.Err() != nil {
			return nil, "", err
		}
		logger.Warn("incremental fetch failed, building the index in full", "base", p.Base[:7], "error", err)
	}
	res, err := gitfetch.Run(ctx, gitfetch.Fetch{CloneURL: p.CloneURL, Token: token, Head: p.Head})
	if err != nil {
		return nil, "", err
	}
	return res, "full", nil
}

func strs(n int) []string { return make([]string, n) }

func baseFor(mode, base string) string {
	if mode == "full" {
		return ""
	}
	return base
}

// stage inserts one batch of chunks under the run, copying tenant_id from
// the run row the same way context packs do.
func stage(ctx context.Context, tx pgx.Tx, runID string, chunks []indexer.Chunk) error {
	n := len(chunks)
	paths, langs, symbols, kinds, scopes, texts := strs(n), strs(n), strs(n), strs(n), strs(n), strs(n)
	starts, ends := make([]int32, n), make([]int32, n)
	for i, c := range chunks {
		paths[i], starts[i], ends[i] = c.Path, int32(c.StartLine), int32(c.EndLine)
		langs[i], symbols[i], kinds[i], scopes[i], texts[i] = c.Language, c.Symbol, c.Kind, c.Scope, c.Text
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO index_staging (runner_run_id, tenant_id, path, start_line, end_line, language, symbol, kind, scope, text)
		SELECT r.id, r.tenant_id, c.path, c.start_line, c.end_line, c.language, c.symbol, c.kind, c.scope, c.text
		FROM runner_runs r, unnest($2::text[], $3::int[], $4::int[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[])
			AS c (path, start_line, end_line, language, symbol, kind, scope, text)
		WHERE r.id = $1`, runID, paths, starts, ends, langs, symbols, kinds, scopes, texts)
	if err != nil {
		return fmt.Errorf("runner: stage chunks: %w", err)
	}
	return nil
}
