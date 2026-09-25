package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/runner"
)

// agentFetchHeadroom is the Job time an agentic run keeps for fetching and
// the context pack on top of the agent's own timeout.
const agentFetchHeadroom = 5 * time.Minute

// agentDeadline bounds an agentic runner Job: the tenant's runner deadline,
// unless the agent's timeout plus the fetch headroom needs longer.
func agentDeadline(runnerDeadline, agentTimeout time.Duration) time.Duration {
	return max(runnerDeadline, agentTimeout+agentFetchHeadroom)
}

// agentRun is the agent_runs row a runner wrote.
type agentRun struct {
	stop    agent.StopReason
	result  []byte
	steps   int
	usage   model.Usage
	costUSD float64
	model   string
	errText string
}

// stopError is nil for a run that submitted a review, and otherwise the
// review's error.
func (r agentRun) stopError() error {
	switch {
	case r.stop == agent.StopSubmitted && len(r.result) > 0:
		return nil
	case r.stop == agent.StopSubmitted:
		return errors.New("agent stopped: submitted without a result")
	case r.errText != "":
		return fmt.Errorf("agent stopped: %s: %s", r.stop, r.errText)
	}
	return fmt.Errorf("agent stopped: %s", r.stop)
}

// response is the run's usage in the shape the usage table and metrics
// take from a single-mode call.
func (r agentRun) response() model.CompletionResponse {
	return model.CompletionResponse{
		Model: r.model, InputTokens: r.usage.Prompt(), CachedTokens: r.usage.CacheRead, OutputTokens: r.usage.Output, CostUSD: r.costUSD,
	}
}

// admission is what an agentic review holds before its runner starts.
type admission struct {
	lease    *lease
	endpoint *runner.ModelEndpoint
	key      string
}

// agentAdmit settles what an agentic review may spend before its runner
// starts, since the runner spends against the model itself: a review model
// must be configured, the tenant's caps must allow a review, and a model
// lease is taken, renewed until released. A non-empty status ends the
// review before it runs, for the reason given.
func (w *Review) agentAdmit(
	ctx context.Context, file *configfile.File, tenant *configfile.Tenant, settings configfile.Settings, jobID int64,
) (admission, string, string, error) {
	ref := settings.Models.Review
	if ref == "" {
		return admission{}, statusSkipped, "no review model is configured for this repository", nil
	}
	endpoint, key, err := modelEndpoint(file, settings.Models)
	if err != nil {
		return admission{}, statusFailed, err.Error(), nil
	}
	capped, err := capReached(ctx, w.Store, tenant.ID(), settings.Limits)
	if err != nil {
		return admission{}, "", "", err
	}
	if capped != "" {
		return admission{}, statusCapped, capped, nil
	}
	slots := settings.Limits.Concurrency
	if slots <= 0 {
		slots = configfile.DefaultConcurrency
	}
	waited := time.Now()
	l, err := acquireLease(ctx, w.Store, tenant.ID(), string(ref), slots, jobID)
	if err != nil {
		return admission{}, "", "", err
	}
	w.Metrics.LeaseWait(tenant.Slug, string(ref), time.Since(waited))
	return admission{lease: l, endpoint: endpoint, key: key}, "", "", nil
}

// modelEndpoint is the review model an agentic runner talks to, and its
// key. A fallback on another provider is not tried in agentic mode: the
// runner holds one key.
func modelEndpoint(file *configfile.File, models configfile.Models) (*runner.ModelEndpoint, string, error) {
	ref := models.Review
	provider, ok := file.Providers[ref.Provider()]
	if !ok {
		return nil, "", fmt.Errorf("worker: provider %q is not in the configuration", ref.Provider())
	}
	var fallbacks []string
	if fb := models.Fallback; fb != "" && fb.Provider() == ref.Provider() {
		fallbacks = []string{fb.Model()}
	}
	return &runner.ModelEndpoint{
		Provider: provider.Type, BaseURL: provider.BaseURL, Model: ref.Model(), Fallbacks: fallbacks, Pricing: provider.Pricing,
	}, provider.APIKeyValue().Value(), nil
}

// agentPrompt reads what the runner's prompt states about the pull request,
// and for a bot author the patch id of its last prepared review, which the
// worker would skip as unchanged.
func (w *Review) agentPrompt(
	ctx context.Context, tenantID string, pr *pullRequest, settings configfile.Settings, prior priorReview,
) (*runner.Prompt, error) {
	p := &runner.Prompt{
		Repository: pr.repository, Number: pr.number, BaseRef: pr.baseRef,
		Instructions: settings.Review.Instructions, RequireSuggestedFix: settings.Review.RequireSuggestedFix,
		MaxDeltaFiles: settings.Incremental.MaxDeltaFiles, Prior: reviewFindings(prior.findings),
	}
	err := w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT title, author, body FROM pull_requests WHERE id = $1`, pr.id).
			Scan(&p.Title, &p.Author, &p.Body); err != nil {
			return fmt.Errorf("worker: read pull request: %w", err)
		}
		if !pr.authorIsBot {
			return nil
		}
		err := tx.QueryRow(ctx, `SELECT patch_id FROM reviews WHERE pull_request_id = $1
			AND status IN ('prepared', 'completed') ORDER BY created_at DESC LIMIT 1`, pr.id).Scan(&p.UnchangedPatchID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("worker: read last review: %w", err)
		}
		return nil
	})
	return p, err
}

// runAgentic publishes what the runner's agent submitted, as run does for
// a single model call. An agent that stopped without submitting fails the
// review, and the sticky comment says this head was not fully reviewed so
// an earlier verdict does not stand in for it.
func (p *publishPhase) runAgentic(ctx context.Context) (string, error) {
	var (
		run  agentRun
		stop string
		diff string
	)
	err := p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT diff FROM context_packs WHERE runner_run_id = $1`, p.runID).Scan(&diff); err != nil {
			return fmt.Errorf("worker: read context pack: %w", err)
		}
		err := tx.QueryRow(ctx, `SELECT stop_reason, result::text, steps, input_tokens, cache_read_tokens, cache_write_tokens,
			output_tokens, cost_usd::float8, model, error FROM agent_runs WHERE runner_run_id = $1`, p.runID).
			Scan(&stop, &run.result, &run.steps, &run.usage.Input, &run.usage.CacheRead, &run.usage.CacheWrite,
				&run.usage.Output, &run.costUSD, &run.model, &run.errText)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("worker: the runner wrote no agent run")
		}
		if err != nil {
			return fmt.Errorf("worker: read agent run: %w", err)
		}
		return nil
	})
	if err != nil {
		return statusFailed, err
	}
	run.stop = agent.StopReason(stop)
	resp := run.response()
	ref := p.settings.Models.Review
	stopErr := run.stopError()
	p.w.Metrics.ModelCall(p.tenant.Slug, string(ref), roleReview, callOutcome(stopErr),
		resp.InputTokens, resp.CachedTokens, resp.OutputTokens, resp.CostUSD)
	p.logger.Info("agent answered", "stop", run.stop, "steps", run.steps, "model", run.model, "input_tokens", resp.InputTokens,
		"cached_tokens", resp.CachedTokens, "output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	if stopErr != nil {
		return statusFailed, errors.Join(stopErr, p.incomplete(ctx, "agent stopped: "+string(run.stop), resp))
	}
	res, dropped, err := review.Parse(string(run.result), review.Anchors(diff), p.parse)
	if err != nil {
		return statusFailed, errors.Join(err, p.incomplete(ctx, "the submitted review was invalid", resp))
	}
	for _, d := range dropped {
		p.logger.Debug("finding dropped", "reason", d.Reason, "path", d.Finding.Path, "line", d.Finding.Line, "title", d.Finding.Title)
	}
	commentID, inline, err := p.writeBack(ctx, res, run.model, append(reviewNotes(nil, dropped), p.repoNotes...))
	if err != nil {
		return statusFailed, err
	}
	p.countFindings(res)
	if err := p.persist(ctx, res, inline, resp, roleReview, commentID); err != nil {
		return statusFailed, err
	}
	return statusCompleted, nil
}

// incomplete replaces the sticky comment with one saying why this head was
// not fully reviewed, in kritik's own template, and records what the run
// spent.
func (p *publishPhase) incomplete(ctx context.Context, reason string, resp model.CompletionResponse) error {
	body, _ := review.RenderSummary(ctx, review.Templates{}, review.RenderData{
		Number: p.pr.number, HeadSHA: p.pr.headSHA, Model: resp.Model, Incomplete: reason, Notes: p.repoNotes,
	})
	commentID, err := p.upsertSticky(ctx, body)
	if err != nil {
		return err
	}
	owner, repo, _ := strings.Cut(p.pr.repository, "/")
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusSuccess, "kritik: review incomplete ("+reason+")"); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
	return p.persist(ctx, review.Result{}, nil, resp, roleReview, commentID)
}
