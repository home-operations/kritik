package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritika/internal/agent"
	"github.com/home-operations/kritika/internal/contextpack"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

// submitReview is the tool whose input is the review contract.
const submitReview = "submit_review"

const submitDescription = "Submit the review and end it. The input is the whole review: a summary and the findings, " +
	"each anchored to a line added or changed on the head side of the diff. Call it exactly once, when you are done."

// SubmitTool is the submit_review tool as the agent is offered it: its
// input is the review contract, strict when a suggested fix is required.
// The bench sends it too, so it measures what a review sends.
func SubmitTool(strict bool) model.ToolDef {
	schema := review.Schema()
	if strict {
		schema = review.SchemaStrict()
	}
	return model.ToolDef{Name: submitReview, Description: submitDescription, InputSchema: schema}
}

// packView is the context pack as the review prompt reads it.
type packView struct {
	Diff      string
	Changed   []string
	Context   []contextpack.Chunk
	DeltaDiff string
	Scope     review.Scope
}

// Skip reasons the runner decides beyond repoconfig's: a bot's pull request
// whose patch id equals its last prepared review's, and a diff that changes
// more lines than the repository's maxChangedLines.
const (
	SkipUnchangedPatch = "unchanged_patch"
	SkipTooLarge       = "too_large"
)

// Notes the runner adds to the pack about the repository's files, which
// the review's summary states.
const (
	noteInstructionsTruncated = "AGENTS.md and CLAUDE.md files truncated to 32 KiB"
	noteRulesLeft             = "%d review rules left out, past the 16 KiB of rule text or 32 KiB of rule files a review is given"
	noteDiffOmitted           = "%d diff file(s) left out of the prompt to fit its budget: %s"
	noteContextOmitted        = "%d context chunk(s) left out of the prompt to fit its budget"
	noteDiffNotKept           = "%d diff file(s) too large to keep, so findings in them have no line to attach to: %s"
)

// promptInputs is what the repository's files and the settings give the
// review prompt for this change: the rules and reference files that apply
// to its paths, and the instructions of its agent files. notes say what
// was left out.
type promptInputs struct {
	rules        []review.Rule
	instructions []string
	references   []review.Reference
	notes        []string
}

// ruleIDs is the ids of the rules the prompt was given.
func (in promptInputs) ruleIDs() []string {
	ids := make([]string, len(in.rules))
	for i, r := range in.rules {
		ids[i] = r.ID
	}
	return ids
}

// newPromptInputs selects, for a change of the changed paths, the spec's
// rules, whose whenExpr the worker has already judged, the context files
// and, when the spec asks, the agent files of the changed directories.
func newPromptInputs(p Spec, files repoconfig.Files, changed []string) promptInputs {
	var in promptInputs
	var agentFiles []string
	if p.AgentFiles {
		agentFiles = repoconfig.AgentFiles(files, changed)
	}
	var truncated bool
	if in.instructions, truncated = repoconfig.Instructions(files, agentFiles); truncated {
		in.notes = append(in.notes, noteInstructionsTruncated)
	}
	var left int
	if in.rules, left = repoconfig.ActiveRules(p.Prompt.Rules, files, changed); left > 0 {
		in.notes = append(in.notes, fmt.Sprintf(noteRulesLeft, left))
	}
	for _, c := range repoconfig.ActiveContext(p.Prompt.Context, changed) {
		in.references = append(in.references, review.Reference{Path: c.Path, Description: c.Description})
	}
	return in
}

// agentPrompt is what the agent is sent: the system prompt, the first user
// message, and whether the contract requires a suggested fix. omitted
// names the diff files, and contextOmitted counts the chunks, the budget
// left out of the message.
type agentPrompt struct {
	system, user   string
	strict         bool
	omitted        []string
	contextOmitted int
}

// noteOmittedPaths is how many omitted paths a note names.
const noteOmittedPaths = 5

// namePaths names the first noteOmittedPaths of paths and counts the rest.
func namePaths(paths []string) string {
	if n := len(paths); n > noteOmittedPaths {
		paths = append(slices.Clone(paths[:noteOmittedPaths]), fmt.Sprintf("and %d more", n-noteOmittedPaths))
	}
	return strings.Join(paths, ", ")
}

// notes are what the summary states about the prompt's cuts.
func (a agentPrompt) notes() []string {
	var notes []string
	if n := len(a.omitted); n > 0 {
		notes = append(notes, fmt.Sprintf(noteDiffOmitted, n, namePaths(a.omitted)))
	}
	if a.contextOmitted > 0 {
		notes = append(notes, fmt.Sprintf(noteContextOmitted, a.contextOmitted))
	}
	return notes
}

// newAgentPrompt composes the prompt from the inputs and the pack, whose
// context includes the similar code the gateway found. commands are what
// the run tool offers, and search says search_code is offered.
func newAgentPrompt(p Spec, in promptInputs, pack packView, commands []string, search bool) agentPrompt {
	system := review.SystemPrompt(in.rules, in.instructions, commands, p.Prompt.Focused, search)
	var incremental *review.IncrementalInput
	if pack.Scope == review.ScopeIncremental {
		incremental = &review.IncrementalInput{PriorHeadSHA: p.PriorHead, DeltaDiff: pack.DeltaDiff, Prior: p.Prompt.Prior}
	}
	pr := p.Prompt.PullRequest
	user, omitted, contextOmitted := review.Build(review.Input{
		Repository: p.Prompt.Repository, Number: pr.Number, Title: pr.Title, Author: pr.Author, Body: pr.Body,
		Issues: p.Prompt.Issues, BaseRef: pr.BaseRef, Changed: pack.Changed, Diff: pack.Diff, Context: pack.Context,
		Incremental: incremental, Dismissed: p.Prompt.Dismissed, References: in.references, BudgetTokens: review.UserBudget(system),
	})
	return agentPrompt{system: system, user: user, strict: p.Prompt.RequireSuggestedFix, omitted: omitted, contextOmitted: contextOmitted}
}

// agentSkip returns why the worker will skip this review whatever the
// agent finds, or "": decided before the agent runs, so a skipped review
// spends nothing.
func agentSkip(p Spec, changed []string, patchID, diff string) string {
	switch {
	case p.Prompt.UnchangedPatchID != "" && patchID == p.Prompt.UnchangedPatchID:
		return SkipUnchangedPatch
	case repoconfig.AllIgnored(p.Ignore, changed):
		return string(repoconfig.SkipOnlyPaths)
	case p.Prompt.MaxChangedLines > 0 && contextpack.ChangedLines(diff, p.Ignore) > p.Prompt.MaxChangedLines:
		return SkipTooLarge
	}
	return ""
}

// limits are the agent loop's bounds, defaults filled in.
func (a *AgentLimits) limits() agent.Limits {
	return agent.Limits{MaxSteps: a.MaxSteps, MaxToolOutputBytes: a.MaxToolOutputBytes, MaxTokens: a.MaxTokens}.WithDefaults()
}

// reviewAgent runs the tool loop over head, with extra tools beside the
// read-only ones. A positive timeout bounds it; running out of time ends
// it as canceled with the timeout in Err.
func reviewAgent(
	ctx context.Context, stepper model.Stepper, p Spec, head *object.Tree, ignore []string, extra []agent.Tool,
	system, user string, strict bool, timeout time.Duration, logger *slog.Logger,
) (agent.Result, []store.TimelineStep) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	limits := p.Agent.limits()
	tree := agent.NewTree(head, ignore)
	issues := make(map[int]string, len(p.Prompt.Issues))
	for _, is := range p.Prompt.Issues {
		issues[is.Number] = is.Body
	}
	timeline := []store.TimelineStep{}
	res := agent.Run{
		Stepper: stepper, Model: p.Model.Model, System: system, User: user,
		Tools: append([]agent.Tool{
			agent.ReadFileTool(tree, limits.MaxToolOutputBytes),
			agent.GrepTool(tree, limits.MaxToolOutputBytes),
			agent.ListFilesTool(tree, limits.MaxToolOutputBytes),
			agent.ReadDescriptionTool(p.Prompt.PullRequest.Body, issues, limits.MaxToolOutputBytes),
		}, extra...),
		Submit:   SubmitTool(strict),
		Validate: review.Check,
		Limits:   limits,
		OnStep: func(e agent.StepEvent) {
			tools := e.Tools
			if tools == nil {
				tools = []string{}
			}
			timeline = append(timeline, store.TimelineStep{
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

// agentTools is what the agent gets beside the read-only tools: the run
// tool, when commands are offered, and search_code, when the repository
// has an index.
type agentTools struct {
	run    *agent.RunTool
	search *searchTool
}

// extra lists the tools to offer.
func (t agentTools) extra() []agent.Tool {
	var out []agent.Tool
	if t.run != nil {
		out = append(out, t.run)
	}
	if t.search != nil {
		out = append(out, t.search)
	}
	return out
}

// commands names the run tool's commands, none without it.
func (t agentTools) commands() []string {
	if t.run == nil {
		return nil
	}
	return t.run.Names()
}

// gatewayRetries is how many times a step the gateway answered with a 500,
// its own database not answering, is sent again; the gateway marks every
// other refusal final, since it has already retried the provider.
const gatewayRetries = 2

// runAgentic runs the agent over the fetched head with the prompt already
// composed, writes its agent_runs row and marks the run done.
func runAgentic(
	ctx context.Context, st *store.Store, p Spec, secrets Secrets, head *object.Tree, ignore []string, tools agentTools,
	prompt agentPrompt, scope review.Scope, logger *slog.Logger,
) error {
	stepper, err := model.NewOpenAI(model.OpenAIConfig{
		BaseURL: strings.TrimSuffix(p.Model.GatewayURL, "/") + "/v1", APIKey: secrets.GatewayToken, ReportsModel: true,
		Retries: gatewayRetries,
	})
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	commands := tools.commands()
	logger.Info("agent started", "model", p.Model.Model, "scope", scope, "prompt_chars", len(prompt.system)+len(prompt.user),
		"commands", commands, "search", tools.search != nil)
	res, timeline := reviewAgent(ctx, stepper, p, head, ignore, tools.extra(), prompt.system, prompt.user, prompt.strict,
		time.Duration(p.Agent.TimeoutSeconds)*time.Second, logger)
	sources := []string{}
	if tools.run != nil {
		sources = tools.run.Sources()
	}
	if cerr := ctx.Err(); cerr != nil {
		// The run was cancelled, deleted or ran out of Job time: what the
		// agent spent so far is still spent, so the row is written on a
		// context of its own, short enough for the pod's termination grace.
		res.Stop = agent.StopCanceled
		if res.Err == "" {
			res.Err = context.Cause(ctx).Error()
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), canceledWriteTimeout)
		defer cancel()
		rec, err := newAgentRecord(res, timeline, sources, secrets)
		if err == nil {
			err = writeAgentRun(wctx, st, p, rec, "failed")
		}
		logger.Warn("agent canceled", "steps", res.Steps, "input_tokens", res.Usage.Prompt(), "output_tokens", res.Usage.Output,
			"cost_usd", res.CostUSD, "error", err)
		return errors.Join(fmt.Errorf("runner: agent: %w", cerr), err)
	}
	rec, err := newAgentRecord(res, timeline, sources, secrets)
	if err != nil {
		return err
	}
	logger.Info("agent stopped", "stop", res.Stop, "steps", res.Steps, "tool_calls", res.ToolCalls, "sources", len(sources),
		"input_tokens", res.Usage.Prompt(), "output_tokens", res.Usage.Output, "cost_usd", res.CostUSD, "error", rec.err)
	return writeAgentRun(ctx, st, p, rec, "done")
}

// canceledWriteTimeout bounds writing a cancelled agent's row, well inside
// a runner pod's termination grace period.
const canceledWriteTimeout = 5 * time.Second

// agentRecord is an agent_runs row.
type agentRecord struct {
	stop                         agent.StopReason
	result                       any
	steps                        int
	toolCalls, timeline, sources []byte
	usage                        model.Usage
	costUSD                      float64
	// model answered the run's last step; empty when no step was answered,
	// and the worker then names the model the run was granted.
	model string
	err   string
}

// newAgentRecord encodes a finished Run and the sources its commands
// fetched. The error text and the sources are masked: an error may carry a
// token, and the worker shows both.
func newAgentRecord(res agent.Result, timeline []store.TimelineStep, sources []string, secrets Secrets) (agentRecord, error) {
	rec := agentRecord{stop: res.Stop, steps: res.Steps, usage: res.Usage, costUSD: res.CostUSD, model: res.Model, err: secrets.Mask(res.Err)}
	masked := make([]string, len(sources))
	for i, s := range sources {
		masked[i] = secrets.Mask(s)
	}
	var err error
	if rec.sources, err = json.Marshal(masked); err != nil {
		return agentRecord{}, fmt.Errorf("runner: encode sources: %w", err)
	}
	if rec.toolCalls, err = json.Marshal(res.ToolCalls); err != nil {
		return agentRecord{}, fmt.Errorf("runner: encode tool calls: %w", err)
	}
	if rec.timeline, err = json.Marshal(timeline); err != nil {
		return agentRecord{}, fmt.Errorf("runner: encode timeline: %w", err)
	}
	if res.Stop == agent.StopSubmitted {
		rec.result = string(res.Submitted)
	}
	return rec, nil
}

// writeAgentRun records the agent's row and moves the run to phase.
func writeAgentRun(ctx context.Context, st *store.Store, p Spec, rec agentRecord, phase string) error {
	return st.WithRunnerJob(ctx, p.RunID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO agent_runs (runner_run_id, account_id, stop_reason, result, steps, tool_calls, timeline,
				input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, cost_usd, model, error, sources)
			SELECT id, account_id, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12, left($13, 2000), $14 FROM runner_runs WHERE id = $1`,
			p.RunID, string(rec.stop), rec.result, rec.steps, rec.toolCalls, rec.timeline,
			rec.usage.Input, rec.usage.CacheRead, rec.usage.CacheWrite, rec.usage.Output, rec.costUSD, rec.model, rec.err,
			rec.sources)
		if err != nil {
			return fmt.Errorf("runner: write agent run: %w", err)
		}
		return setPhaseTx(ctx, tx, p.RunID, phase)
	})
}
