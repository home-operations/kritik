package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
)

// Similarity retrieval bounds: neighbours per query, chunks kept, and the
// cosine similarity floor below which a neighbour is noise.
const (
	similarPerQuery = 4
	similarMax      = 10
	similarFloor    = 0.5
)

// similarRequest is one search of a repository's index.
type similarRequest struct {
	account                *configfile.Account
	repositoryID, reviewID string
	// jobID is the review's job, for which the embedding slot is held.
	jobID int64
	slots int
	// queries are the texts to embed, each already within
	// contextpack.SimilarQueryChars; exclude are the paths left out of the
	// answer.
	queries []string
	exclude []string
}

// similar searches the repository's active index generation for the
// chunks nearest each query, excluding the given paths. indexed is false,
// with no chunks, when the instance has no embedder or the repository no
// completed generation built with it. It returns the tokens the embedding
// spent, recorded as the review's usage.
func (b *Base) similar(
	ctx context.Context, embedders *Embedders, file *configfile.File, r similarRequest, logger *slog.Logger,
) (chunks []contextpack.Chunk, tokens int64, indexed bool, err error) {
	embedder, emb := embedders.Embedder(file)
	if embedder == nil {
		return nil, 0, false, nil
	}
	var runID string
	err = b.Store.WithAccount(ctx, r.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT r.active_index_run_id::text FROM repositories r JOIN index_runs g ON g.id = r.active_index_run_id
			WHERE r.id = $1 AND g.status = 'completed' AND g.embed_model = $2 AND g.embed_dims = $3`,
			r.repositoryID, emb.Model, emb.Dims).Scan(&runID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("worker: active index: %w", err)
	}
	if len(r.queries) == 0 {
		return nil, 0, true, nil
	}
	var vectors [][]float32
	err = b.withLease(ctx, r.account, "embed:"+emb.Model, r.slots, r.jobID, func(ctx context.Context) error {
		var err error
		vectors, tokens, err = embedder.Embed(ctx, r.queries)
		b.Metrics.ModelCall(r.account.Key(), emb.Model, store.RoleEmbedding, callOutcome(err), tokens, 0, 0, 0)
		return err
	})
	if err != nil {
		return nil, 0, true, err
	}
	type hit struct {
		chunk contextpack.Chunk
		sim   float64
	}
	var hits []hit
	seen := map[string]bool{}
	err = b.Store.WithAccount(ctx, r.account.ID(), func(tx pgx.Tx) error {
		if err := store.InsertUsage(ctx, tx, store.Usage{
			AccountID: r.account.ID(), RepositoryID: r.repositoryID, ReviewID: r.reviewID,
			Role: store.RoleEmbedding, Model: emb.Model, Input: tokens,
		}); err != nil {
			return err
		}
		// The generation and account filters are strict and cheap, exactly
		// what VectorChord's prefilter wants: it then skips the distance of
		// every chunk outside this repository's generation.
		if _, err := tx.Exec(ctx, `SET LOCAL vchordrq.prefilter = on`); err != nil {
			return fmt.Errorf("worker: enable prefilter: %w", err)
		}
		for _, v := range vectors {
			rows, err := tx.Query(ctx, `SELECT path, start_line, end_line, language, symbol, kind, scope, text, 1 - (embedding <=> $1::halfvec)
				FROM index_chunks WHERE index_run_id = $2 AND NOT (path = ANY($3))
				ORDER BY embedding <=> $1::halfvec LIMIT $4`, model.VectorLiteral(v), runID, r.exclude, similarPerQuery)
			if err != nil {
				return fmt.Errorf("worker: similar chunks: %w", err)
			}
			for rows.Next() {
				var c contextpack.Chunk
				var sim float64
				if err := rows.Scan(&c.Path, &c.StartLine, &c.EndLine, &c.Language, &c.Symbol, &c.Kind, &c.Scope, &c.Text, &sim); err != nil {
					rows.Close()
					return err
				}
				key := fmt.Sprintf("%s:%d", c.Path, c.StartLine)
				if sim < similarFloor || seen[key] {
					continue
				}
				seen[key] = true
				c.Stage = contextpack.StageSimilar
				c.Ref = fmt.Sprintf("similarity %.2f", sim)
				hits = append(hits, hit{c, sim})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, tokens, true, err
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Compare(b.sim, a.sim) })
	if len(hits) > similarMax {
		hits = hits[:similarMax]
	}
	chunks = make([]contextpack.Chunk, 0, len(hits))
	for _, h := range hits {
		chunks = append(chunks, h.chunk)
	}
	logger.Info("similar chunks", "queries", len(r.queries), "kept", len(chunks), "tokens", tokens)
	return chunks, tokens, true, nil
}
