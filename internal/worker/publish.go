package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
)

// CompleterSource resolves a configured provider name to a Completer.
type CompleterSource interface {
	For(f *configfile.File, name string) (model.Completer, error)
}

// maxOutputTokens bounds one review answer. Findings are short by
// instruction; this is a guard against a runaway model, not a target.
const maxOutputTokens = 4096

// Usage roles, matching the usage table's CHECK.
const (
	roleReview    = "review"
	roleFallback  = "fallback"
	roleEmbedding = "embedding"
)

// reviewInput is what the model phase reads from the database.
type reviewInput struct {
	diff    string
	changed []string
	context []contextpack.Chunk
	title   string
	author  string
}

// publishPhase runs the model over a prepared review and writes the answer
// back to the forge and the database. It returns the final review status
// and, for a failure the caller should surface, the error.
type publishPhase struct {
	w        *Review
	file     *configfile.File
	tenant   *configfile.Tenant
	settings configfile.Settings
	client   forge.Client
	pr       *pullRequest
	reviewID string
	runID    string
	jobID    int64
	logger   *slog.Logger
}

func (p *publishPhase) run(ctx context.Context) (status string, err error) {
	ref := p.settings.Models.Review
	if ref == "" {
		return statusSkipped, errors.New("no review model is configured for this repository")
	}
	capped, err := p.checkCaps(ctx)
	if err != nil {
		return statusFailed, err
	}
	if capped != "" {
		p.logger.Warn("review capped", "cap", capped)
		return statusCapped, errors.New(capped)
	}
	in, err := p.load(ctx)
	if err != nil {
		return statusFailed, err
	}
	similar, err := p.similar(ctx, in)
	if err != nil {
		// Stage 4 is best effort: the index may be absent or mid-rebuild.
		p.logger.Warn("similar-code retrieval skipped", "error", err)
	}
	in.context = append(in.context, similar...)
	msg, omitted, contextOmitted := review.Build(review.Input{
		Repository: p.pr.repository, Number: p.pr.number, Title: in.title, Author: in.author,
		BaseRef: p.pr.baseRef, Changed: in.changed, Diff: in.diff, Context: in.context,
	})
	p.logger.Info("prompt built", "chars", len(msg), "diff_files_omitted", len(omitted),
		"context_chunks", len(in.context), "context_omitted", contextOmitted)
	byStage := map[string]int{}
	for _, c := range in.context[:len(in.context)-min(contextOmitted, len(in.context))] {
		byStage[c.Stage]++
	}
	for stage, n := range byStage {
		p.w.Metrics.ContextChunks(p.tenant.Slug, stage, n)
	}

	resp, role, err := p.complete(ctx, ref, msg)
	if err != nil {
		return statusFailed, err
	}
	res, dropped, err := review.Parse(resp.Raw, review.Anchors(in.diff))
	if err != nil {
		return statusFailed, err
	}
	p.logger.Info("model answered", "model", resp.Model, "upstream", resp.Upstream, "findings", len(res.Findings),
		"dropped", len(dropped), "omitted", len(omitted), "input_tokens", resp.InputTokens, "cached_tokens", resp.CachedTokens,
		"output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	for _, f := range dropped {
		p.logger.Debug("finding dropped", "path", f.Path, "line", f.Line, "title", f.Title)
	}

	commentID, err := p.writeBack(ctx, res, resp.Model, omitted, len(dropped))
	if err != nil {
		return statusFailed, err
	}
	bySeverity := map[string]int{}
	for _, f := range res.Findings {
		bySeverity[string(f.Severity)]++
	}
	for severity, n := range bySeverity {
		p.w.Metrics.Findings(p.tenant.Slug, severity, n)
	}
	if err := p.persist(ctx, res, resp, role, commentID); err != nil {
		return statusFailed, err
	}
	return statusCompleted, nil
}

// checkCaps returns a description of the cap that is exhausted, or "".
func (p *publishPhase) checkCaps(ctx context.Context) (string, error) {
	limits := p.settings.Limits
	if limits.ReviewsPerDay <= 0 && limits.TokensPerMonth <= 0 {
		return "", nil
	}
	var reviews, tokens int64
	err := p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reviews
			WHERE status = 'completed' AND created_at >= date_trunc('day', now())`).Scan(&reviews); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(sum(input_tokens + output_tokens), 0) FROM usage
			WHERE created_at >= date_trunc('month', now())`).Scan(&tokens)
	})
	if err != nil {
		return "", fmt.Errorf("worker: read caps: %w", err)
	}
	if limits.ReviewsPerDay > 0 && reviews >= int64(limits.ReviewsPerDay) {
		return fmt.Sprintf("reviewsPerDay (%d) reached", limits.ReviewsPerDay), nil
	}
	if limits.TokensPerMonth > 0 && tokens >= limits.TokensPerMonth {
		return fmt.Sprintf("tokensPerMonth (%d) reached", limits.TokensPerMonth), nil
	}
	return "", nil
}

func (p *publishPhase) load(ctx context.Context) (reviewInput, error) {
	var in reviewInput
	err := p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		var stages []byte
		if err := tx.QueryRow(ctx, `SELECT diff, changed_paths, stages FROM context_packs WHERE runner_run_id = $1`, p.runID).
			Scan(&in.diff, &in.changed, &stages); err != nil {
			return fmt.Errorf("worker: read context pack: %w", err)
		}
		// Packs written before the context stages existed hold '{}'.
		if len(stages) > 0 && stages[0] == '[' {
			if err := json.Unmarshal(stages, &in.context); err != nil {
				return fmt.Errorf("worker: decode context pack: %w", err)
			}
		}
		if err := tx.QueryRow(ctx, `SELECT title, author FROM pull_requests WHERE id = $1`, p.pr.id).Scan(&in.title, &in.author); err != nil {
			return fmt.Errorf("worker: read pull request: %w", err)
		}
		return nil
	})
	return in, err
}

// complete calls the primary model under a lease, falling back to the
// configured fallback model. A fallback on the same provider is handed to
// the provider (OpenRouter switches server-side); one on another provider
// is a second call from here. The returned role says which answered.
func (p *publishPhase) complete(ctx context.Context, ref configfile.ModelRef, msg string) (model.CompletionResponse, string, error) {
	var resp model.CompletionResponse
	role := roleReview
	err := p.w.withLease(ctx, p.tenant, string(ref), p.settings.Slots(), p.jobID, func(ctx context.Context) error {
		var err error
		resp, role, err = p.callModels(ctx, ref, msg)
		return err
	})
	return resp, role, err
}

// callModels asks the primary model and, when configured on another
// provider, the fallback. The caller holds the lease.
func (p *publishPhase) callModels(ctx context.Context, ref configfile.ModelRef, msg string) (model.CompletionResponse, string, error) {
	req := model.CompletionRequest{
		System: review.System, User: msg, Model: ref.Model(),
		Schema: review.Schema(), SchemaName: "findings", MaxTokens: maxOutputTokens,
	}
	fallback := p.settings.Models.Fallback
	if fallback != "" && fallback.Provider() == ref.Provider() {
		req.Fallbacks = []string{fallback.Model()}
	}
	completer, err := p.w.Completers.For(p.file, ref.Provider())
	if err != nil {
		return model.CompletionResponse{}, "", err
	}
	resp, err := completer.Complete(ctx, req)
	p.w.Metrics.ModelCall(p.tenant.Slug, string(ref), roleReview, callOutcome(err),
		resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD)
	if err == nil || fallback == "" || fallback.Provider() == ref.Provider() || ctx.Err() != nil {
		return resp, roleReview, err
	}
	p.logger.Warn("primary model failed, trying fallback", "model", ref, "fallback", fallback, "error", err)
	fc, ferr := p.w.Completers.For(p.file, fallback.Provider())
	if ferr != nil {
		return model.CompletionResponse{}, "", errors.Join(err, ferr)
	}
	req.Model, req.Fallbacks = fallback.Model(), nil
	resp, ferr = fc.Complete(ctx, req)
	p.w.Metrics.ModelCall(p.tenant.Slug, string(fallback), roleFallback, callOutcome(ferr),
		resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD)
	if ferr != nil {
		return model.CompletionResponse{}, "", errors.Join(err, ferr)
	}
	return resp, roleFallback, nil
}

// writeBack posts the sticky comment (created once, edited after), the
// inline review, and the commit status. Only the sticky comment is
// required: the other two are best effort and logged when they fail, so a
// forge quirk cannot turn a finished review into a retry storm.
func (p *publishPhase) writeBack(ctx context.Context, res review.Result, modelName string, omitted []string, dropped int) (int64, error) {
	owner, repo, _ := strings.Cut(p.pr.repository, "/")
	login, err := p.client.BotLogin(ctx)
	if err != nil {
		return 0, err
	}
	marker := review.Marker(p.pr.number)
	body := review.StickyBody(p.pr.number, res, modelName, omitted, dropped)

	var commentID int64
	_ = p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT forge_comment_id FROM sticky_comments WHERE pull_request_id = $1`, p.pr.id).Scan(&commentID)
	})
	if commentID == 0 {
		if commentID, err = p.client.FindComment(ctx, owner, repo, p.pr.number, login, marker); err != nil {
			return 0, err
		}
	}
	if commentID != 0 {
		err = p.client.UpdateComment(ctx, owner, repo, commentID, body)
	} else {
		commentID, err = p.client.CreateComment(ctx, owner, repo, p.pr.number, body)
	}
	if err != nil {
		return 0, err
	}

	inline := make([]forge.InlineComment, 0, len(res.Findings))
	for _, f := range res.Findings {
		inline = append(inline, forge.InlineComment{Path: f.Path, Line: f.Line, Body: review.InlineBody(f)})
	}
	if err := p.client.CreateReview(ctx, owner, repo, p.pr.number, p.pr.headSHA, inline); err != nil {
		p.logger.Warn("inline review not posted", "error", err)
	}
	desc := "no findings"
	if n := len(res.Findings); n > 0 {
		desc = fmt.Sprintf("%d finding(s)", n)
	}
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusSuccess, "kritik: "+desc); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
	return commentID, nil
}

func (p *publishPhase) persist(ctx context.Context, res review.Result, resp model.CompletionResponse, role string, commentID int64) error {
	return p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		for _, f := range res.Findings {
			if _, err := tx.Exec(ctx, `INSERT INTO findings (tenant_id, review_id, path, line, severity, title, body)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.tenant.ID(), p.reviewID, f.Path, f.Line, string(f.Severity), f.Title, f.Body); err != nil {
				return fmt.Errorf("worker: insert finding: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sticky_comments (pull_request_id, tenant_id, forge_comment_id) VALUES ($1, $2, $3)
			ON CONFLICT (pull_request_id) DO UPDATE SET forge_comment_id = excluded.forge_comment_id, updated_at = now()`,
			p.pr.id, p.tenant.ID(), commentID); err != nil {
			return fmt.Errorf("worker: upsert sticky comment: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO usage
			(tenant_id, repository_id, review_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			p.tenant.ID(), p.pr.repositoryID, p.reviewID, role, resp.Model, resp.Upstream,
			resp.InputTokens, resp.OutputTokens, resp.CostUSD); err != nil {
			return fmt.Errorf("worker: insert usage: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE reviews SET model = $2 WHERE id = $1`, p.reviewID, resp.Model); err != nil {
			return fmt.Errorf("worker: record model: %w", err)
		}
		return nil
	})
}

// Similarity retrieval bounds: hunks embedded per review, neighbours per
// hunk, chunks kept, and the cosine similarity floor below which a
// neighbour is noise.
const (
	similarHunks    = 12
	similarPerHunk  = 4
	similarMax      = 10
	similarFloor    = 0.5
	similarHunkChar = 3000
)

// similar is stage 4: the diff's hunks are embedded and the nearest chunks
// of the repository's active index generation are pulled in, excluding
// the changed paths, which the overlay already covers.
func (p *publishPhase) similar(ctx context.Context, in reviewInput) ([]contextpack.Chunk, error) {
	if p.w.Embedder == nil {
		return nil, nil
	}
	var runID string
	err := p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT r.active_index_run_id::text FROM repositories r JOIN index_runs g ON g.id = r.active_index_run_id
			WHERE r.id = $1 AND g.status = 'completed' AND g.embed_model = $2`, p.pr.repositoryID, p.w.EmbedModel).Scan(&runID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: active index: %w", err)
	}
	hunks := contextpack.Hunks(in.diff)
	if len(hunks) > similarHunks {
		hunks = hunks[:similarHunks]
	}
	if len(hunks) == 0 {
		return nil, nil
	}
	texts := make([]string, len(hunks))
	for i, h := range hunks {
		t := h.Path + "\n" + h.Text
		if len(t) > similarHunkChar {
			t = t[:similarHunkChar]
		}
		texts[i] = t
	}
	var vectors [][]float32
	var tokens int64
	err = p.w.withLease(ctx, p.tenant, "embed:"+p.w.EmbedModel, p.settings.Slots(), p.jobID, func(ctx context.Context) error {
		var err error
		vectors, tokens, err = p.w.Embedder.Embed(ctx, texts)
		p.w.Metrics.ModelCall(p.tenant.Slug, p.w.EmbedModel, roleEmbedding, callOutcome(err), tokens, 0, 0, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	type hit struct {
		chunk contextpack.Chunk
		sim   float64
	}
	var hits []hit
	seen := map[string]bool{}
	err = p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO usage (tenant_id, repository_id, review_id, role, model, input_tokens)
			VALUES ($1, $2, $3, 'embedding', $4, $5)`, p.tenant.ID(), p.pr.repositoryID, p.reviewID, p.w.EmbedModel, tokens); err != nil {
			return fmt.Errorf("worker: record embedding usage: %w", err)
		}
		for _, v := range vectors {
			rows, err := tx.Query(ctx, `SELECT path, start_line, end_line, language, symbol, kind, scope, text, 1 - (embedding <=> $1::halfvec)
				FROM index_chunks WHERE index_run_id = $2 AND NOT (path = ANY($3))
				ORDER BY embedding <=> $1::halfvec LIMIT $4`, model.VectorLiteral(v), runID, in.changed, similarPerHunk)
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
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].sim > hits[j].sim })
	if len(hits) > similarMax {
		hits = hits[:similarMax]
	}
	out := make([]contextpack.Chunk, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.chunk)
	}
	p.logger.Info("similar chunks", "hunks", len(hunks), "kept", len(out), "tokens", tokens)
	return out, nil
}

func callOutcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
