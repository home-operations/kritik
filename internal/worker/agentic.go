package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
)

// agentDeadline bounds an agentic runner Job: the tenant's runner deadline,
// unless the agent's timeout plus the fetch headroom needs longer.
func agentDeadline(runnerDeadline, agentTimeout time.Duration) time.Duration {
	return max(runnerDeadline, agentTimeout+jobtimeout.AgentFetchHeadroom)
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
	sources []string
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
	lease *lease
	// maxTokens is the agent's token budget for this review.
	maxTokens int64
}

// agentAdmit settles what an agentic review may spend before its runner
// starts, since the runner spends against the model through the gateway:
// the gateway must be configured, a review model must be, the tenant's
// caps must allow a review, and a free model lease is taken, renewed until
// released, or errNoSlot returned. A non-empty status ends the review
// before it runs, for the reason given.
func (w *Review) agentAdmit(
	ctx context.Context, logger *slog.Logger, file *configfile.File, tenant *configfile.Tenant, settings configfile.Settings, jobID int64,
) (admission, string, string, error) {
	ref := settings.Models.Review
	if ref == "" {
		return admission{}, statusSkipped, "no review model is configured for this repository", nil
	}
	if w.GatewayURL == "" {
		return admission{}, statusFailed, "agentic mode needs the model gateway (KRITIK_GATEWAY_URL)", nil
	}
	if _, ok := file.Providers[ref.Provider()]; !ok {
		return admission{}, statusFailed, fmt.Sprintf("worker: provider %q is not in the configuration", ref.Provider()), nil
	}
	l, err := takeLease(ctx, w.Store, tenant.ID(), string(ref), settings.Limits.Concurrency, jobID)
	if err != nil {
		return admission{}, "", "", err
	}
	if l == nil {
		return admission{}, "", "", errNoSlot
	}
	// The caps are read under the lease, so concurrent reviews cannot all
	// pass a cap of one.
	budget, capped, err := w.agentCaps(ctx, tenant, settings)
	if err != nil || capped != "" {
		w.releaseLease(ctx, logger, l, string(ref))
		if err != nil {
			return admission{}, "", "", err
		}
		return admission{}, statusCapped, capped, nil
	}
	return admission{lease: l, maxTokens: budget}, "", "", nil
}

// agentCaps is the token budget an agentic review may spend, or the cap
// that stops it.
func (w *Review) agentCaps(ctx context.Context, tenant *configfile.Tenant, settings configfile.Settings) (int64, string, error) {
	limits := settings.Limits
	if limits.ReviewsPerDay <= 0 && limits.TokensPerMonth <= 0 {
		return settings.Agent.MaxTokens, "", nil
	}
	u, err := readUsage(ctx, w.Store, tenant.ID())
	if err != nil {
		return 0, "", err
	}
	if capped := u.reached(limits); capped != "" {
		return 0, capped, nil
	}
	budget, capped := agentBudget(settings.Agent.MaxTokens, limits.TokensPerMonth, u.tokens)
	return budget, capped, nil
}

// minAgentTokens is the least monthly headroom an agentic review starts
// with: below it the agent could not read the diff before running out.
const minAgentTokens = 50_000

// agentBudget is how many tokens one agentic review may spend: the
// repository's agent budget, cut to what is left of the tenant's monthly
// cap when one is set. A non-empty reason caps the review instead, when
// too little is left for an agent to do anything with.
func agentBudget(agentMax, tokensPerMonth, usedThisMonth int64) (int64, string) {
	if tokensPerMonth <= 0 {
		return agentMax, ""
	}
	left := tokensPerMonth - usedThisMonth
	if left <= minAgentTokens {
		return 0, fmt.Sprintf("tokensPerMonth (%d) nearly reached: %d tokens left", tokensPerMonth, max(left, 0))
	}
	return min(agentMax, left), ""
}

// agentPrompt reads what the runner needs to write the prompt and to tell
// a review the worker will skip: the pull request as the repository filter
// sees it and, for a bot author, the patch id of its last prepared review,
// which afterRun skips as unchanged.
func (w *Review) agentPrompt(
	ctx context.Context, tenantID, reviewID, trigger string, pr *pullRequest, settings configfile.Settings, prior priorReview,
) (*runner.Prompt, error) {
	p := &runner.Prompt{
		Repository: pr.repository, Instructions: settings.Review.Instructions, RequireSuggestedFix: settings.Review.RequireSuggestedFix,
		MaxDeltaFiles: settings.Incremental.MaxDeltaFiles, Prior: reviewFindings(prior.findings),
	}
	err := w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		if p.PullRequest, err = loadFilterPR(ctx, tx, pr.id); err != nil {
			return err
		}
		if !pr.authorIsBot || trigger == jobs.TriggerManual {
			return nil
		}
		err = tx.QueryRow(ctx, `SELECT patch_id FROM reviews WHERE pull_request_id = $1 AND id <> $2
			AND status IN ('prepared', 'completed') ORDER BY created_at DESC LIMIT 1`, pr.id, reviewID).Scan(&p.UnchangedPatchID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("worker: read last review: %w", err)
		}
		return nil
	})
	p.Trim()
	return p, err
}

// loadAgentRun reads the agent_runs row of a runner run; found is false
// when the runner wrote none.
func (w *Review) loadAgentRun(ctx context.Context, tenantID, runID string) (run agentRun, found bool, err error) {
	var stop string
	err = w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT stop_reason, result::text, steps, input_tokens, cache_read_tokens, cache_write_tokens,
			output_tokens, cost_usd::float8, model, error, sources FROM agent_runs WHERE runner_run_id = $1`, runID).
			Scan(&stop, &run.result, &run.steps, &run.usage.Input, &run.usage.CacheRead, &run.usage.CacheWrite,
				&run.usage.Output, &run.costUSD, &run.model, &run.errText, &run.sources)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return agentRun{}, false, nil
	}
	if err != nil {
		return agentRun{}, false, fmt.Errorf("worker: read agent run: %w", err)
	}
	run.stop = agent.StopReason(stop)
	return run, true, nil
}

// readAgentRun reads the run's agent_runs row, nil when the runner wrote
// none. What the agent spent is already charged: the gateway records usage
// for every step it serves, whatever becomes of the review. A run no step
// of which was answered names ref's model, the one it was granted.
//
// A run that ended in error may still be writing its row: a deleted runner
// pod records how its agent stopped while it terminates. await waits for
// that, until the run settles or agentRowWait passes. ctx's cancellation is
// not inherited, so a job River cancels still reads the row.
func (w *Review) readAgentRun(ctx context.Context, tenantID, runID string, ref configfile.ModelRef, await bool) (*agentRun, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentRowWait+10*time.Second)
	defer cancel()
	run, found, err := w.loadAgentRun(ctx, tenantID, runID)
	if err == nil && !found && await {
		run, found, err = w.awaitAgentRun(ctx, tenantID, runID)
	}
	if err != nil || !found {
		return nil, err
	}
	if run.model == "" {
		run.model = ref.Model()
	}
	return &run, nil
}

// gatewayModel is the name an agentic runner calls its model by; the
// gateway maps it to the provider model the run was granted.
const gatewayModel = "review"

// agentSpec makes spec an agentic run: the prompt, the gateway and a run
// token for it, and the agent's bounds. The token is minted last, so an
// error leaves none behind; the caller revokes it once the run ends. It
// returns the runner Job's deadline, which the agent's timeout may
// lengthen.
func (w *Review) agentSpec(
	ctx context.Context, tenantID, reviewID, runID, trigger string, pr *pullRequest, settings configfile.Settings, prior priorReview,
	admitted admission, spec *runner.Spec, secrets *runner.Secrets, deadline time.Duration,
) (time.Duration, error) {
	prompt, err := w.agentPrompt(ctx, tenantID, reviewID, trigger, pr, settings, prior)
	if err != nil {
		return deadline, err
	}
	deadline = agentDeadline(deadline, settings.Agent.Timeout)
	token, err := w.Store.MintGatewayToken(ctx, store.GatewayGrant{
		RunID: runID, TenantID: tenantID, ReviewID: reviewID, RepositoryID: pr.repositoryID,
		Model: string(settings.Models.Review), Fallback: string(settings.Models.Fallback), Budget: admitted.maxTokens,
	}, time.Now().Add(deadline+w.GatewayTokenTTL))
	if err != nil {
		return deadline, err
	}
	spec.Prompt, spec.Mode, secrets.GatewayToken = prompt, runner.ModeAgentic, token
	spec.Model = &runner.ModelEndpoint{GatewayURL: w.GatewayURL, Model: gatewayModel}
	spec.Agent = &runner.AgentLimits{
		MaxSteps: settings.Agent.MaxSteps, MaxToolOutputBytes: settings.Agent.MaxToolOutputBytes, MaxTokens: admitted.maxTokens,
		TimeoutSeconds: int(settings.Agent.Timeout / time.Second),
		Commands:       settings.Agent.Commands, CommandTimeoutSeconds: int(settings.Agent.CommandTimeout / time.Second),
	}
	return deadline, nil
}

// revokeGatewayTokens ends the run's token once its runner is done, on a
// context of its own since the job's may have ended. A token not revoked
// still expires on its own.
func (w *Review) revokeGatewayTokens(ctx context.Context, logger *slog.Logger, runID string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := w.Store.RevokeGatewayTokens(rctx, runID); err != nil {
		logger.Warn("gateway token not revoked", "error", err)
	}
}

// stopped reports whether a run was stopped from outside, by supervision,
// the job's context or the Job's deadline, rather than ending on its own.
func stopped(ctx context.Context, res executor.Result, cause error) bool {
	return res.Err != nil && (cause != nil || ctx.Err() != nil || res.DeadlineExceeded)
}

// agentRowWait is how long a failed run's agent row is waited for: a
// runner pod's termination grace period.
const agentRowWait = 30 * time.Second

// agentRowPoll is how often awaitAgentRun looks again.
const agentRowPoll = time.Second

// awaitAgentRun polls for a run's agent_runs row until it appears, the
// run's phase settles without one, or agentRowWait passes.
func (w *Review) awaitAgentRun(ctx context.Context, tenantID, runID string) (agentRun, bool, error) {
	deadline := time.After(agentRowWait)
	for {
		var phase string
		err := w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT phase FROM runner_runs WHERE id = $1`, runID).Scan(&phase)
		})
		if err != nil {
			return agentRun{}, false, fmt.Errorf("worker: read run phase: %w", err)
		}
		// The runner writes its agent row before it settles the phase.
		settled := phase == "done" || phase == "failed"
		run, found, err := w.loadAgentRun(ctx, tenantID, runID)
		if err != nil || found || settled {
			return run, found, err
		}
		select {
		case <-deadline:
			return agentRun{}, false, nil
		case <-ctx.Done():
			return agentRun{}, false, nil
		case <-time.After(agentRowPoll):
		}
	}
}

// runAgentic publishes what the runner's agent submitted, as run does for
// a single model call; the run's usage is already recorded. An agent that
// stopped without submitting fails the review, and the sticky comment says
// this head was not fully reviewed so an earlier verdict does not stand in
// for it. A run the runner skipped ends the review skipped.
func (p *publishPhase) runAgentic(ctx context.Context) (string, error) {
	// The agent has already answered, so publishing runs to the end even if
	// the job's ctx ends meanwhile, as run does once its model answers.
	ctx, cancel := detach(ctx)
	defer cancel()
	if p.agent == nil {
		return statusFailed, errors.New("worker: the runner wrote no agent run")
	}
	run := *p.agent
	if run.stop == runner.AgentSkipped {
		p.logger.Info("review "+statusSkipped+" by the runner", "reason", run.errText)
		p.skippedStatus(ctx, run.errText)
		if reason := repoconfig.SkipReason(run.errText); reason.Valid() {
			err := p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE reviews SET skip_reason = $2 WHERE id = $1`, p.reviewID, string(reason))
				return err
			})
			if err != nil {
				return statusFailed, fmt.Errorf("worker: record skip reason: %w", err)
			}
		}
		return statusSkipped, nil
	}
	var diff string
	err := p.w.Store.WithTenant(ctx, p.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT diff FROM context_packs WHERE runner_run_id = $1`, p.runID).Scan(&diff)
	})
	if err != nil {
		return statusFailed, fmt.Errorf("worker: read context pack: %w", err)
	}
	resp := run.response()
	p.logger.Info("agent answered", "stop", run.stop, "steps", run.steps, "model", run.model, "input_tokens", resp.InputTokens,
		"cached_tokens", resp.CachedTokens, "output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	if stopErr := run.stopError(); stopErr != nil {
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

// skippedStatus says on the head why the runner skipped its review: the
// worker only reaches the runner's skip when its own checks did not skip,
// and so did not say so itself.
func (p *publishPhase) skippedStatus(ctx context.Context, reason string) {
	desc := repoconfig.SkipReason(reason).Description()
	if reason == runner.SkipUnchangedPatch {
		desc = "patch unchanged since the last review"
	}
	owner, repo, _ := strings.Cut(p.pr.repository, "/")
	if err := p.client.SetStatus(ctx, owner, repo, p.pr.headSHA, forge.StatusSuccess, "kritik: skipped ("+desc+")"); err != nil {
		p.logger.Warn("commit status not set", "error", err)
	}
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
