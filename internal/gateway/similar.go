package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/adapter"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/textcut"
)

// Similarity retrieval bounds: neighbours per query, chunks kept, and the
// cosine similarity floor below which a neighbour is noise.
const (
	similarPerQuery = 4
	similarMax      = 10
	similarFloor    = 0.5
)

// maxSimilarBody bounds a similar-code request: its queries, each cut to
// contextpack.SimilarQueryChars, with room for their encoding.
const maxSimilarBody = 256 << 10

// similarCode searches the run's repository index for the request's
// queries (ADR-0026 §2.2): stage 4 of the review, asked once over the
// diff's hunks before the agent starts, and the agent's own search_code
// tool. The embedding is reserved against the run's budget at four
// characters a token before it runs; what it returns is code of the
// repository the runner has already checked out.
func (g *Server) similarCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := g.admit(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSimilarBody))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		refuse(w, http.StatusRequestEntityTooLarge, "invalid_request", err.Error())
		return
	}
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", "reading the request: "+err.Error())
		return
	}
	var req contextpack.SimilarRequest
	if err := json.Unmarshal(body, &req); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", "decoding the request: "+err.Error())
		return
	}
	if len(req.Queries) == 0 || len(req.Queries) > contextpack.SimilarQueries {
		refuse(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("the request must carry between 1 and %d queries, not %d", contextpack.SimilarQueries, len(req.Queries)))
		return
	}
	var chars int
	for i, q := range req.Queries {
		if strings.TrimSpace(q) == "" {
			refuse(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("query %d is empty", i))
			return
		}
		req.Queries[i] = textcut.Prefix(q, contextpack.SimilarQueryChars)
		chars += len(req.Queries[i])
	}
	var jobID int64
	err = g.Store.WithAccount(ctx, c.grant.AccountID, func(tx pgx.Tx) error {
		var err error
		jobID, err = store.ReviewJobID(ctx, tx, c.grant.ReviewID)
		return err
	})
	if err != nil {
		c.logger.Error("gateway: review not read", "error", err)
		refuse(w, http.StatusInternalServerError, "server_error", "the run's review could not be read")
		return
	}
	reserved := int64(chars)/4 + 1
	if !g.reserve(ctx, w, c, reserved) {
		return
	}
	chunks, tokens, indexed, err := g.similar(ctx, c.file, similarRequest{
		account: c.account, repositoryID: c.grant.RepositoryID, reviewID: c.grant.ReviewID, jobID: jobID,
		slots: c.file.Settings(c.account, "").Limits.Concurrency, queries: req.Queries, exclude: req.Exclude,
	}, c.logger)
	cctx, cancel := detach(ctx)
	defer cancel()
	if cerr := g.Store.ChargeGatewayToken(cctx, c.token, tokens-reserved); cerr != nil {
		c.logger.Error("gateway: similar code not charged", "error", cerr)
	}
	if err != nil {
		// The embedder's error may carry its key or URL; the runner only
		// learns that the search failed.
		c.logger.Warn("gateway: similar code failed", "error", err)
		refuse(w, http.StatusBadGateway, "upstream_error", "similar-code retrieval failed")
		return
	}
	if chunks == nil {
		chunks = []contextpack.Chunk{}
	}
	out, err := json.Marshal(contextpack.SimilarResponse{Chunks: chunks, Indexed: indexed})
	if err != nil {
		refuse(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

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
func (g *Server) similar(
	ctx context.Context, file *configfile.File, r similarRequest, logger *slog.Logger,
) (chunks []contextpack.Chunk, tokens int64, indexed bool, err error) {
	embedder, emb := g.Embedders.Embedder(file)
	if embedder == nil {
		return nil, 0, false, nil
	}
	var runID string
	err = g.Store.WithAccount(ctx, r.account.ID(), func(tx pgx.Tx) error {
		var err error
		runID, err = store.ActiveGenerationFor(ctx, tx, r.repositoryID, emb.Model, emb.Dims)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if len(r.queries) == 0 {
		return nil, 0, true, nil
	}
	lease, err := g.Store.AcquireLease(ctx, r.account.ID(), "embed:"+emb.Model, r.slots, r.jobID)
	if err != nil {
		return nil, 0, true, err
	}
	var vectors [][]float32
	vectors, tokens, err = embedder.Embed(ctx, r.queries)
	g.Metrics.ModelCall(r.account.Key(), emb.Model, store.RoleEmbedding, adapter.Outcome(err), tokens, 0, 0, 0)
	rctx, cancel := detach(ctx)
	if rerr := lease.Release(rctx); rerr != nil {
		logger.Warn("lease not released", "key", "embed:"+emb.Model, "error", rerr)
	}
	cancel()
	if err != nil {
		return nil, 0, true, err
	}
	var hits []store.SimilarHit
	err = g.Store.WithAccount(ctx, r.account.ID(), func(tx pgx.Tx) error {
		if err := store.InsertUsage(ctx, tx, store.Usage{
			AccountID: r.account.ID(), RepositoryID: r.repositoryID, ReviewID: r.reviewID,
			Role: store.RoleEmbedding, Model: emb.Model, Input: tokens,
		}); err != nil {
			return err
		}
		var err error
		hits, err = store.SimilarChunks(ctx, tx, runID, vectors, r.exclude, similarPerQuery)
		return err
	})
	if err != nil {
		return nil, tokens, true, err
	}
	seen := map[string]bool{}
	kept := make([]store.SimilarHit, 0, len(hits))
	for _, h := range hits {
		key := fmt.Sprintf("%s:%d", h.Chunk.Path, h.Chunk.StartLine)
		if h.Similarity < similarFloor || seen[key] {
			continue
		}
		seen[key] = true
		h.Chunk.Ref = fmt.Sprintf("similarity %.2f", h.Similarity)
		kept = append(kept, h)
	}
	slices.SortStableFunc(kept, func(a, b store.SimilarHit) int { return cmp.Compare(b.Similarity, a.Similarity) })
	if len(kept) > similarMax {
		kept = kept[:similarMax]
	}
	chunks = make([]contextpack.Chunk, 0, len(kept))
	for _, h := range kept {
		chunks = append(chunks, h.Chunk)
	}
	logger.Info("similar chunks", "queries", len(r.queries), "kept", len(chunks), "tokens", tokens)
	return chunks, tokens, true, nil
}
