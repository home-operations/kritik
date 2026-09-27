package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/tasks"
)

// taskSearchK is how many chunks a search source finds when it sets no k.
const taskSearchK = 8

// contextFile is one file of .Context.files.
type contextFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// contextFiles reads the files the task names by path at the commit its
// definition came from. A glob needs a checkout: an agentic run's runner
// gathers it, and a single-mode run notes that it leaves it out.
func (r *taskRunner) contextFiles(ctx context.Context, budget *tasks.Budget) ([]contextFile, error) {
	var out []contextFile
	for _, f := range r.task.Context.Files {
		if f.Path == "" {
			if r.task.RunMode() != tasks.ModeAgentic {
				budget.Notes = append(budget.Notes, fmt.Sprintf("context files %s left out: a glob is gathered in agentic mode only", f.Glob))
			}
			continue
		}
		b, err := r.client.FileAt(ctx, r.owner, r.name, r.args.ConfigSHA, f.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			budget.Notes = append(budget.Notes, fmt.Sprintf("context files %s left out: not found", f.Path))
			continue
		case errors.Is(err, forge.ErrFileTooLarge):
			budget.Notes = append(budget.Notes, fmt.Sprintf("context files %s left out: too large", f.Path))
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, contextFile{Path: f.Path, Content: budget.Take("files "+f.Path, string(b))})
	}
	return out, nil
}

// indexHit is one chunk of the repository index a search source found.
type indexHit struct {
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Symbol    string `json:"symbol,omitempty"`
	Text      string `json:"text"`
}

// searchIndex finds the chunks of the repository's active index nearest q's
// query, nearest first. ok is false, and the reason noted, when there is
// no index to search.
func (r *taskRunner) searchIndex(ctx context.Context, q tasks.NamedQuery) (hits []indexHit, ok bool, err error) {
	w := r.w
	if w.Embedder == nil {
		r.notes = append(r.notes, fmt.Sprintf("context %s left out: repository indexing is off", q.Name))
		return nil, false, nil
	}
	if q.Query == "" {
		r.notes = append(r.notes, fmt.Sprintf("context %s left out: the query rendered empty", q.Name))
		return nil, false, nil
	}
	var runID string
	err = w.Store.WithTenant(ctx, r.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT r.active_index_run_id::text FROM repositories r JOIN index_runs g ON g.id = r.active_index_run_id
			WHERE r.id = $1 AND g.status = 'completed' AND g.embed_model = $2`, r.args.RepositoryID, w.EmbedModel).Scan(&runID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		r.notes = append(r.notes, fmt.Sprintf("context %s left out: the repository has no index yet", q.Name))
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("worker: active index: %w", err)
	}
	var vectors [][]float32
	var tokens int64
	err = w.withLease(ctx, r.tenant, "embed:"+w.EmbedModel, r.settings.Limits.Concurrency, r.jobID, func(ctx context.Context) error {
		var err error
		vectors, tokens, err = w.Embedder.Embed(ctx, []string{q.Query})
		w.Metrics.ModelCall(r.tenant.Slug, w.EmbedModel, roleEmbedding, callOutcome(err), tokens, 0, 0, 0)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if len(vectors) != 1 {
		return nil, false, fmt.Errorf("worker: embed search query: got %d vectors", len(vectors))
	}
	k := q.K
	if k <= 0 {
		k = taskSearchK
	}
	err = w.Store.WithTenant(ctx, r.tenant.ID(), func(tx pgx.Tx) error {
		// A run past its answer never gathers context again; should one,
		// its embedding is not charged twice.
		if r.run.AnsweredAt == nil {
			if err := insertUsage(ctx, tx, reviewUsage{
				tenantID: r.tenant.ID(), repositoryID: r.args.RepositoryID, role: roleEmbedding, model: w.EmbedModel, input: tokens,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL vchordrq.prefilter = on`); err != nil {
			return fmt.Errorf("worker: enable prefilter: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT path, start_line, end_line, symbol, text FROM index_chunks WHERE index_run_id = $2
			ORDER BY embedding <=> $1::halfvec LIMIT $3`, model.VectorLiteral(vectors[0]), runID, k)
		if err != nil {
			return fmt.Errorf("worker: search index: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var h indexHit
			if err := rows.Scan(&h.Path, &h.StartLine, &h.EndLine, &h.Symbol, &h.Text); err != nil {
				return fmt.Errorf("worker: search index: %w", err)
			}
			hits = append(hits, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, err
	}
	return hits, true, nil
}
