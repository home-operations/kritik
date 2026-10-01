package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
)

// ActiveGenerationFor is the repository's active, completed index
// generation built with the given embedder, or ErrNotFound.
func ActiveGenerationFor(ctx context.Context, tx pgx.Tx, repositoryID, embedModel string, dims int) (string, error) {
	var runID string
	err := tx.QueryRow(ctx, `SELECT r.active_index_run_id::text FROM repositories r JOIN index_runs g ON g.id = r.active_index_run_id
		WHERE r.id = $1 AND g.status = 'completed' AND g.embed_model = $2 AND g.embed_dims = $3`, repositoryID, embedModel, dims).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: active generation: %w", err)
	}
	return runID, nil
}

// SimilarHit is one index chunk near a query, with its cosine similarity.
type SimilarHit struct {
	Chunk      contextpack.Chunk
	Similarity float64
}

// SimilarChunks finds, for each query vector, the perQuery chunks of the
// generation nearest it, outside the excluded paths, in the order the
// index ranks them. The generation and account filters are strict and
// cheap, which is what VectorChord's prefilter wants: it then skips the
// distance of every chunk outside this repository's generation.
func SimilarChunks(
	ctx context.Context, tx pgx.Tx, runID string, vectors [][]float32, exclude []string, perQuery int,
) ([]SimilarHit, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL vchordrq.prefilter = on`); err != nil {
		return nil, fmt.Errorf("store: enable prefilter: %w", err)
	}
	var hits []SimilarHit
	for _, v := range vectors {
		rows, err := tx.Query(ctx, `SELECT path, start_line, end_line, language, symbol, kind, scope, text, 1 - (embedding <=> $1::halfvec)
			FROM index_chunks WHERE index_run_id = $2 AND NOT (path = ANY($3))
			ORDER BY embedding <=> $1::halfvec LIMIT $4`, model.VectorLiteral(v), runID, exclude, perQuery)
		if err != nil {
			return nil, fmt.Errorf("store: similar chunks: %w", err)
		}
		hits, err = pgx.AppendRows(hits, rows, func(row pgx.CollectableRow) (SimilarHit, error) {
			var h SimilarHit
			c := &h.Chunk
			err := row.Scan(&c.Path, &c.StartLine, &c.EndLine, &c.Language, &c.Symbol, &c.Kind, &c.Scope, &c.Text, &h.Similarity)
			c.Stage = contextpack.StageSimilar
			return h, err
		})
		if err != nil {
			return nil, fmt.Errorf("store: similar chunks: %w", err)
		}
	}
	return hits, nil
}
