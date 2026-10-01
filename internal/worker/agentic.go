package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/agent"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/executor"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/gateway"
	"github.com/home-operations/kritika/internal/jobtimeout"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/runner"
	"github.com/home-operations/kritika/internal/store"
)

// agentDeadline bounds a review's runner Job: the account's runner deadline,
// unless the agent's timeout plus the fetch headroom needs longer.
func agentDeadline(runnerDeadline, agentTimeout time.Duration) time.Duration {
	return max(runnerDeadline, agentTimeout+jobtimeout.AgentFetchHeadroom)
}

// stopError is nil for a run that submitted a review, and otherwise the
// review's error.
func stopError(r store.AgentRunRow) error {
	stop := agent.StopReason(r.StopReason)
	switch {
	case stop == agent.StopSubmitted && len(r.Result) > 0:
		return nil
	case stop == agent.StopSubmitted:
		return errors.New("agent stopped: submitted without a result")
	case r.Error != "":
		return fmt.Errorf("agent stopped: %s: %s", stop, r.Error)
	}
	return fmt.Errorf("agent stopped: %s", stop)
}

// admission is what a review holds before its runner starts.
type admission struct {
	lease *store.Lease
	// maxTokens is the agent's token budget for this review.
	maxTokens int64
}

// agentAdmit settles what a review may spend before its runner
// starts, since the runner spends against the model through the gateway:
// a review model must be configured, the account's caps must allow a
// review, and a free model lease is taken, renewed until released, or
// errNoSlot returned. A non-empty status ends the review
// before it runs, for the reason given.
func (w *Review) agentAdmit(
	ctx context.Context, logger *slog.Logger, file *configfile.File, account *configfile.Account, settings configfile.Settings, jobID int64,
) (admission, store.ReviewStatus, string, error) {
	ref := settings.Models.Review
	if ref == "" {
		return admission{}, store.ReviewSkipped, "no review model is configured for this repository", nil
	}
	if _, ok := file.Provider(account, ref.Provider()); !ok {
		return admission{}, store.ReviewFailed, fmt.Sprintf("provider %q is not in the configuration", ref.Provider()), nil
	}
	l, err := w.Store.TakeLease(ctx, account.ID(), string(ref), settings.Limits.Concurrency, jobID)
	if err != nil {
		return admission{}, "", "", err
	}
	if l == nil {
		return admission{}, "", "", errNoSlot
	}
	// The caps are read under the lease, so concurrent reviews cannot all
	// pass a cap of one.
	budget, capped, err := w.agentCaps(ctx, account, settings)
	if err != nil || capped != "" {
		w.releaseLease(ctx, logger, l, string(ref))
		if err != nil {
			return admission{}, "", "", err
		}
		return admission{}, store.ReviewCapped, capped, nil
	}
	return admission{lease: l, maxTokens: budget}, "", "", nil
}

// agentCaps is the token budget a review may spend, or the cap
// that stops it.
func (w *Review) agentCaps(ctx context.Context, account *configfile.Account, settings configfile.Settings) (int64, string, error) {
	limits := settings.Limits
	if limits.ReviewsPerDay <= 0 && limits.TokensPerMonth <= 0 {
		return settings.Agent.MaxTokens, "", nil
	}
	u, err := w.Store.AccountUsage(ctx, account.ID())
	if err != nil {
		return 0, "", fmt.Errorf("worker: read caps: %w", err)
	}
	if capped := store.CapReason(u, limits); capped != "" {
		return 0, capped, nil
	}
	budget, capped := agentBudget(settings.Agent.MaxTokens, limits.TokensPerMonth, u.Tokens)
	return budget, capped, nil
}

// minAgentTokens is the least monthly headroom a review starts
// with: below it the agent could not read the diff before running out.
const minAgentTokens = 50_000

// agentBudget is how many tokens one review may spend: the
// repository's agent budget, cut to what is left of the account's monthly
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
// sees it, the issues its description says it closes, and, for a bot
// author, the patch id of its last prepared review, which afterRun skips as
// unchanged. The notes say which issues could not be read.
func (w *Review) agentPrompt(
	ctx context.Context, accountID, reviewID, trigger string, pr *pullRequest, eff Effective, prior priorReview,
	client forge.Client, logger *slog.Logger,
) (*runner.Prompt, []string, error) {
	p := &runner.Prompt{
		Repository: pr.repository, Context: eff.Review.Context,
		RequireSuggestedFix: eff.Review.RequireSuggestedFix, Focused: eff.Review.Focused(),
		MaxDeltaFiles: eff.Incremental.MaxDeltaFiles, Prior: reviewFindings(prior.findings), Dismissed: dismissedFindings(prior.dismissed),
	}
	err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		if p.PullRequest, err = loadFilterPR(ctx, tx, pr.id); err != nil {
			return err
		}
		// The rules' whenExpr is judged here, before the runner cuts the
		// body, so a rule applies as it will when the worker reads the run.
		judged := p.PullRequest
		judged.Event = trigger
		vars, err := judged.Vars()
		if err != nil {
			return err
		}
		p.Rules = repoconfig.RulesFor(eff.Review.Rules, vars)
		if pr.dedupesBotPatch(trigger) {
			p.UnchangedPatchID, err = lastPatchID(ctx, tx, pr.id, reviewID)
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	owner, repo := pr.ownerRepo()
	var notes []string
	p.Issues, notes = linkedIssues(ctx, client, owner, repo, p.PullRequest.Body, logger)
	p.Trim()
	return p, notes, nil
}

// loadAgentRun reads the agent_runs row of a runner run; found is false
// when the runner wrote none.
func (w *Review) loadAgentRun(ctx context.Context, accountID, runID string) (run store.AgentRunRow, found bool, err error) {
	err = w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
		var err error
		run, err = store.FindAgentRun(ctx, tx, runID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.AgentRunRow{}, false, nil
	}
	if err != nil {
		return store.AgentRunRow{}, false, fmt.Errorf("worker: read agent run: %w", err)
	}
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
func (w *Review) readAgentRun(
	ctx context.Context, accountID, runID string, ref configfile.ModelRef, await bool,
) (*store.AgentRunRow, error) {
	wait := cmp.Or(w.rowWait, agentRowWait)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait+10*time.Second)
	defer cancel()
	run, found, err := w.loadAgentRun(ctx, accountID, runID)
	if err == nil && !found && await {
		run, found, err = w.awaitAgentRun(ctx, accountID, runID, wait)
	}
	if err != nil || !found {
		return nil, err
	}
	run.Model = cmp.Or(run.Model, ref.Model())
	return &run, nil
}

// agentSpec gives spec its agent: the prompt, the gateway and a run
// token for it, and the agent's bounds. The token is minted last, so an
// error leaves none behind; the caller revokes it once the run ends. It
// returns the runner Job's deadline, which the agent's timeout may
// lengthen, and the prompt's notes.
func (w *Review) agentSpec(
	ctx context.Context, accountID, reviewID, runID, trigger string, pr *pullRequest, eff Effective, prior priorReview,
	admitted admission, spec *runner.Spec, secrets *runner.Secrets, deadline time.Duration, client forge.Client, logger *slog.Logger,
) (time.Duration, []string, error) {
	settings := eff.Settings
	prompt, notes, err := w.agentPrompt(ctx, accountID, reviewID, trigger, pr, eff, prior, client, logger)
	if err != nil {
		return deadline, nil, err
	}
	deadline = agentDeadline(deadline, settings.Agent.Timeout)
	token, err := w.Store.MintGatewayToken(ctx, store.GatewayGrant{
		RunID: runID, AccountID: accountID, ReviewID: reviewID, RepositoryID: pr.repositoryID,
		Model: string(settings.Models.Review), Fallback: string(settings.Models.Fallback), Budget: admitted.maxTokens,
	}, time.Now().Add(deadline+w.GatewayTokenTTL))
	if err != nil {
		return deadline, nil, err
	}
	spec.Prompt, secrets.GatewayToken = prompt, token
	spec.Model = &runner.ModelEndpoint{GatewayURL: w.GatewayURL, Model: gateway.ModelName}
	spec.Agent = &runner.AgentLimits{
		MaxSteps: settings.Agent.MaxSteps, MaxToolOutputBytes: settings.Agent.MaxToolOutputBytes, MaxTokens: admitted.maxTokens,
		TimeoutSeconds: int(settings.Agent.Timeout / time.Second),
		Commands:       settings.Agent.Commands, CommandTimeoutSeconds: int(settings.Agent.CommandTimeout / time.Second),
	}
	return deadline, notes, nil
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
// run's phase settles without one, or wait passes.
func (w *Review) awaitAgentRun(ctx context.Context, accountID, runID string, wait time.Duration) (store.AgentRunRow, bool, error) {
	deadline := time.After(wait)
	for {
		var phase string
		err := w.Store.WithAccount(ctx, accountID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT phase FROM runner_runs WHERE id = $1`, runID).Scan(&phase)
		})
		if err != nil {
			return store.AgentRunRow{}, false, fmt.Errorf("worker: read run phase: %w", err)
		}
		// The runner writes its agent row before it settles the phase.
		settled := phase == "done" || phase == "failed"
		run, found, err := w.loadAgentRun(ctx, accountID, runID)
		if err != nil || found || settled {
			return run, found, err
		}
		select {
		case <-deadline:
			return store.AgentRunRow{}, false, nil
		case <-ctx.Done():
			return store.AgentRunRow{}, false, nil
		case <-time.After(agentRowPoll):
		}
	}
}
