package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
)

// submitReview is the tool whose input is the review contract.
const submitReview = "submit_review"

const submitDescription = "Submit the review and end it. The input is the whole review: a summary and the findings, " +
	"each anchored to a line added or changed on the head side of the diff. Call it exactly once, when you are done."

// packView is the context pack as the review prompt reads it.
type packView struct {
	Diff      string
	Changed   []string
	Context   []contextpack.Chunk
	DeltaDiff string
	Scope     review.Scope
}

// timelineStep is one agent step as agent_runs.timeline records it.
type timelineStep struct {
	Index        int      `json:"index"`
	Tools        []string `json:"tools"`
	DurationMS   int64    `json:"duration_ms"`
	OutputBytes  int      `json:"output_bytes"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
}

// repoFile parses the merge-base .kritik.yaml; ok is false when there is
// none or it does not parse, in which case the operator's settings stand.
func repoFile(files repoconfig.Files) (repoconfig.File, bool) {
	doc, ok := files[repoconfig.FileName]
	if !ok {
		return repoconfig.File{}, false
	}
	f, _, err := repoconfig.Parse([]byte(doc))
	return f, err == nil
}

// agentPrompt composes the system prompt and user message the way a
// single-mode review does, from the same repository files and pack. Only
// the similar-code stage is missing: the runner has no index, and the
// agent can grep instead. strict says whether the contract requires a
// suggested fix.
func agentPrompt(p Spec, files repoconfig.Files, pack packView) (system, user string, strict bool) {
	paths, strict := p.Prompt.Instructions, p.Prompt.RequireSuggestedFix
	if f, ok := repoFile(files); ok {
		if len(f.Review.Instructions) > 0 {
			paths = f.Review.Instructions
		}
		if f.Review.RequireSuggestedFix != nil {
			strict = *f.Review.RequireSuggestedFix
		}
	}
	instructions, _ := repoconfig.Instructions(files, paths)
	system = review.SystemPrompt(instructions)
	var incremental *review.IncrementalInput
	if pack.Scope == review.ScopeIncremental {
		incremental = &review.IncrementalInput{PriorHeadSHA: p.PriorHead, DeltaDiff: pack.DeltaDiff, Prior: p.Prompt.Prior}
	}
	user, _, _ = review.Build(review.Input{
		Repository: p.Prompt.Repository, Number: p.Prompt.Number, Title: p.Prompt.Title, Author: p.Prompt.Author,
		Body: p.Prompt.Body, BaseRef: p.Prompt.BaseRef, Changed: pack.Changed, Diff: pack.Diff, Context: pack.Context,
		Incremental: incremental, BudgetTokens: review.UserBudget(system),
	})
	return system, user, strict
}

// agentSkip returns why the worker will skip this review whatever the
// agent finds, or "". It mirrors the worker's checks that need no pull
// request fields; a review the in-repo filter rejects still runs.
func agentSkip(p Spec, files repoconfig.Files, changed []string, patchID string) string {
	if p.Prompt.UnchangedPatchID != "" && patchID == p.Prompt.UnchangedPatchID {
		return "unchanged patch"
	}
	if f, ok := repoFile(files); ok {
		if f.Enabled != nil && !*f.Enabled {
			return "disabled"
		}
		if f.Skip.All(changed) {
			return "only skipped paths"
		}
	}
	return ""
}

// reviewAgent runs the tool loop over head. A positive timeout bounds it;
// running out of time ends it as canceled with the timeout in Err.
func reviewAgent(
	ctx context.Context, stepper model.Stepper, p Spec, head *object.Tree, ignore []string,
	system, user string, strict bool, timeout time.Duration, logger *slog.Logger,
) (agent.Result, []timelineStep) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	limits := agent.Limits{
		MaxSteps: p.Agent.MaxSteps, MaxToolOutputBytes: p.Agent.MaxToolOutputBytes, MaxTokens: p.Agent.MaxTokens,
	}.WithDefaults()
	schema := review.Schema()
	if strict {
		schema = review.SchemaStrict()
	}
	tree := agent.NewTree(head, ignore)
	timeline := []timelineStep{}
	res := agent.Run{
		Stepper: stepper, Model: p.Model.Model, Fallbacks: p.Model.Fallbacks, System: system, User: user,
		Tools: []agent.Tool{
			agent.ReadFileTool(tree, limits.MaxToolOutputBytes),
			agent.GrepTool(tree, limits.MaxToolOutputBytes),
			agent.ListFilesTool(tree, limits.MaxToolOutputBytes),
		},
		Submit: model.ToolDef{Name: submitReview, Description: submitDescription, InputSchema: schema},
		Limits: limits,
		OnStep: func(e agent.StepEvent) {
			tools := e.Tools
			if tools == nil {
				tools = []string{}
			}
			timeline = append(timeline, timelineStep{
				Index: e.Index, Tools: tools, DurationMS: e.Duration.Milliseconds(), OutputBytes: e.OutputBytes,
				InputTokens: e.Usage.Prompt(), OutputTokens: e.Usage.Output,
			})
			logger.Info("agent step", "step", e.Index, "tools", tools, "duration", e.Duration.Round(time.Millisecond),
				"output_bytes", e.OutputBytes, "input_tokens", e.Usage.Prompt(), "output_tokens", e.Usage.Output)
		},
	}.Do(actx)
	if res.Stop == agent.StopCanceled && ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded) {
		res.Err = fmt.Sprintf("agent timeout (%s) reached", timeout)
	}
	return res, timeline
}

// runAgentic runs the agent over the fetched head and writes its
// agent_runs row, then marks the run done. A review the worker will skip
// anyway writes no row.
func runAgentic(
	ctx context.Context, st *store.Store, p Spec, secrets Secrets, head *object.Tree, files repoconfig.Files,
	pack packView, ignore []string, patchID string, logger *slog.Logger,
) error {
	if reason := agentSkip(p, files, pack.Changed, patchID); reason != "" {
		logger.Info("agent not run", "reason", reason)
		return setPhase(ctx, st, p.RunID, "done")
	}
	stepper, err := model.NewStepper(p.Model.Provider, p.Model.BaseURL, secrets.ModelAPIKey, p.Model.Pricing, nil)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	system, user, strict := agentPrompt(p, files, pack)
	logger.Info("agent started", "model", p.Model.Model, "scope", pack.Scope, "prompt_chars", len(system)+len(user))
	res, timeline := reviewAgent(ctx, stepper, p, head, ignore, system, user, strict,
		time.Duration(p.Agent.TimeoutSeconds)*time.Second, logger)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("runner: agent: %w", err)
	}
	logger.Info("agent stopped", "stop", res.Stop, "steps", res.Steps, "tool_calls", res.ToolCalls,
		"input_tokens", res.Usage.Prompt(), "output_tokens", res.Usage.Output, "cost_usd", res.CostUSD, "error", res.Err)

	toolCalls, err := json.Marshal(res.ToolCalls)
	if err != nil {
		return fmt.Errorf("runner: encode tool calls: %w", err)
	}
	timelineJSON, err := json.Marshal(timeline)
	if err != nil {
		return fmt.Errorf("runner: encode timeline: %w", err)
	}
	var result any
	if res.Stop == agent.StopSubmitted {
		result = string(res.Submitted)
	}
	return st.WithRunnerJob(ctx, p.RunID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO agent_runs (runner_run_id, tenant_id, stop_reason, result, steps, tool_calls, timeline,
				input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, cost_usd, model, error)
			SELECT id, tenant_id, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12, left($13, 2000) FROM runner_runs WHERE id = $1`,
			p.RunID, string(res.Stop), result, res.Steps, toolCalls, timelineJSON,
			res.Usage.Input, res.Usage.CacheRead, res.Usage.CacheWrite, res.Usage.Output, res.CostUSD, p.Model.Model, res.Err)
		if err != nil {
			return fmt.Errorf("runner: write agent run: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE runner_runs SET phase = 'done' WHERE id = $1`, p.RunID)
		return err
	})
}
