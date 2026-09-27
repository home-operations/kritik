package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
)

// agentic runs the task's agent in a runner over the commit the task was
// defined at, with the prompts and answer schema rendered here, and
// concludes on what it submitted as a single-mode run does on its answer.
// The runner reaches its model only through the gateway, holds only the
// read-only git token, and writes nothing to the forge: every write is the
// plan's, made here. An error before the runner starts is retried; once it
// has started, the run ends here.
func (r *taskRunner) agentic(ctx context.Context, data tasks.PromptData, labels []string) (store.TaskRunResult, error) {
	ref, fallback := r.models()
	system, user, err := r.prepared.RenderPrompt(data)
	if err != nil {
		return failed("", err), nil
	}
	schema, err := json.Marshal(r.prepared.AnswerSchema(labels))
	if err != nil {
		return failed("", fmt.Errorf("worker: encode answer schema: %w", err)), nil
	}
	gitToken, err := r.client.GitToken(ctx)
	if err != nil {
		return store.TaskRunResult{}, err
	}
	// The caps are read under the model slot agenticSlot took, so
	// concurrent runs cannot all pass a cap of one.
	budget, capped, err := r.agentBudget(ctx)
	if err != nil {
		return store.TaskRunResult{}, err
	}
	if capped != "" {
		return skipped(capped), nil
	}
	return r.runAgent(ctx, ref, fallback, budget, r.taskPrompt(system, user, schema), gitToken, labels)
}

// agenticSlot settles whether an agentic run can start and takes a slot on
// its model without waiting, as an agentic review does, before the run does
// anything billable: a busy model snoozes the job, and a snoozed job has
// spent nothing, not even an index search's embedding, when it comes back.
// release is nil when the run does not go on, for the result or error
// given; otherwise the caller holds the slot until the run ends.
func (r *taskRunner) agenticSlot(ctx context.Context) (store.TaskRunResult, func(), error) {
	if r.w.GatewayURL == "" || r.w.Executor == nil {
		return failed("", errors.New("worker: agentic tasks need the model gateway (KRITIK_GATEWAY_URL)")), nil, nil
	}
	ref, _ := r.models()
	if ref == "" {
		return skipped(noModel), nil, nil
	}
	if _, ok := r.file.Providers[ref.Provider()]; !ok {
		return failed("", fmt.Errorf("worker: provider %q is not in the configuration", ref.Provider())), nil, nil
	}
	l, err := takeLease(ctx, r.w.Store, r.tenant.ID(), string(ref), r.settings.Limits.Concurrency, r.jobID)
	if err != nil {
		return store.TaskRunResult{}, nil, err
	}
	if l == nil {
		return store.TaskRunResult{}, nil, r.snooze(string(ref))
	}
	return store.TaskRunResult{}, func() { r.w.releaseLease(ctx, r.logger, l, string(ref)) }, nil
}

// snooze puts the job back for later, backing off with each snooze, when
// every slot of the task's model is held.
func (r *taskRunner) snooze(modelKey string) error {
	d := backoff(r.snoozes, snoozeMin, snoozeMax)
	r.logger.Info("task snoozed: every model slot is held", "model", modelKey, "snoozes", r.snoozes+1, "for", d.Round(time.Second))
	return river.JobSnooze(d)
}

// agentBudget is the tokens the task's agent may spend, cut to what is
// left of the tenant's monthly cap, or the cap that stops it.
func (r *taskRunner) agentBudget(ctx context.Context) (int64, string, error) {
	maxTokens := r.settings.Agent.MaxTokens
	if t := r.task.Agent.MaxTokens; t != nil {
		maxTokens = *t
	}
	limits := r.settings.Limits
	if limits.TokensPerMonth <= 0 {
		return maxTokens, "", nil
	}
	u, err := readUsage(ctx, r.w.Store, r.tenant.ID())
	if err != nil {
		return 0, "", err
	}
	budget, capped := agentBudget(maxTokens, limits.TokensPerMonth, u.tokens)
	return budget, capped, nil
}

// taskPrompt is what the runner needs beyond the checkout: the rendered
// prompts and schema, the tools and commands the task uses, and the
// context sources only a checkout can gather, within what the worker's
// sources left of the context budget.
func (r *taskRunner) taskPrompt(system, user string, schema []byte) *runner.TaskPrompt {
	t := r.task
	p := &runner.TaskPrompt{
		Name: t.Name, System: system, User: user, Schema: schema, SourceBytes: taskSourceBytes, ContextBytes: r.contextLeft,
	}
	tools := t.Agent.Tools
	if tools == nil {
		tools = r.settings.TaskBounds.Tools
	}
	for _, tool := range tools {
		if slices.Contains(runner.TaskTools, tool) {
			p.Tools = append(p.Tools, tool)
		}
	}
	// The run tool takes both the tool and commands for it. A nil
	// agent.commands, which chooseTask also leaves when it drops a list
	// out of bounds, offers no command rather than the operator's own.
	if slices.Contains(tools, runTool) {
		p.Run = t.Agent.Commands
	}
	for _, f := range t.Context.Files {
		if f.Glob != "" {
			p.Files = append(p.Files, runner.TaskFiles{Glob: f.Glob, Max: f.Max})
		}
	}
	for _, c := range t.Context.Commands {
		p.Commands = append(p.Commands, runner.TaskCommand{Name: c.Name, Argv: strings.Fields(c.Run)})
	}
	return p
}

// runTool is the agent tool that runs the task's agent.commands.
const runTool = "run"

// taskCommands are the binaries a task's runner may execute: those its
// agent's run tool offers and those its context commands run.
func taskCommands(p *runner.TaskPrompt) []string {
	commands := slices.Clone(p.Run)
	for _, c := range p.Commands {
		commands = append(commands, c.Argv[0])
	}
	slices.Sort(commands)
	return slices.Compact(commands)
}

// runAgent starts the runner, waits for it under supervision and concludes
// on the agent's answer. Its error, retried, is one from before the
// runner starts.
func (r *taskRunner) runAgent(
	ctx context.Context, ref, fallback configfile.ModelRef, budget int64, prompt *runner.TaskPrompt, gitToken string, labels []string,
) (store.TaskRunResult, error) {
	tenantID := r.tenant.ID()
	var runID string
	err := r.w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		runID, err = store.InsertTaskRunner(ctx, tx, tenantID, r.run.ID)
		return err
	})
	if err != nil {
		return store.TaskRunResult{}, err
	}
	logger := r.logger.With("runner_run", runID)
	deadline, resources := r.file.RunnerFor(r.tenant)
	agent := r.settings.Agent
	if t := r.task.Agent.MaxSteps; t != nil {
		agent.MaxSteps = *t
	}
	if t := r.task.Agent.MaxToolOutputBytes; t != nil {
		agent.MaxToolOutputBytes = *t
	}
	if t := r.task.Agent.Timeout; t != nil {
		agent.Timeout = *t
	}
	deadline = agentDeadline(deadline, agent.Timeout)
	commands := taskCommands(prompt)
	spec := runner.Spec{
		Version: runner.SpecVersion, Kind: runner.KindTask, RunID: runID, CloneURL: r.client.CloneURL(r.owner, r.name),
		Head: r.args.ConfigSHA, Ignore: r.settings.Ignore, Mode: runner.ModeAgentic,
		Model: &runner.ModelEndpoint{GatewayURL: r.w.GatewayURL, Model: gatewayModel},
		Agent: &runner.AgentLimits{
			MaxSteps: agent.MaxSteps, MaxToolOutputBytes: agent.MaxToolOutputBytes, MaxTokens: budget,
			TimeoutSeconds: int(agent.Timeout / time.Second), Commands: commands, CommandTimeoutSeconds: int(agent.CommandTimeout / time.Second),
		},
		Task: prompt,
	}
	if err := spec.Validate(); err != nil {
		r.runnerNotStarted(ctx, runID, err)
		return failed("", err), nil
	}
	// The token is minted last, so an error leaves none behind.
	token, err := r.w.Store.MintGatewayToken(ctx, store.GatewayGrant{
		RunID: runID, TenantID: tenantID, TaskRunID: r.run.ID, RepositoryID: r.args.RepositoryID,
		Model: string(ref), Fallback: string(fallback), Budget: budget,
	}, time.Now().Add(deadline+r.w.GatewayTokenTTL))
	if err != nil {
		r.runnerNotStarted(ctx, runID, err)
		return store.TaskRunResult{}, err
	}
	logger.Info("task runner starting", "model", ref, "budget", budget, "commands", commands)
	sup := runSupervision(r.w.Store, tenantID, runID, "", "", r.w.superviseEvery, logger)
	res, cause := supervise(ctx, sup, r.w.Executor, executor.Spec{
		RunID: runID,
		Labels: map[string]string{
			labelTenant: r.tenant.Slug, labelRepository: r.repo.name, "task": r.task.Name, labelKind: jobs.QueueTask,
		},
		Annotations: map[string]string{annotationJob: strconv.FormatInt(r.jobID, 10), annotationHead: r.args.ConfigSHA},
		Job:         spec,
		Secrets:     runner.Secrets{GitToken: gitToken, GatewayToken: token},
		Deadline:    deadline,
		Resources:   resources,
		Tools:       r.file.ToolsFor(commands),
	})
	r.w.revokeGatewayTokens(ctx, logger, runID)
	// The agent's row is read before recordRun settles the run's phase: a
	// stopped run's row may still be on its way from the terminating pod.
	run, agentErr := r.w.readAgentRun(ctx, tenantID, runID, ref, stopped(ctx, res, cause))
	ctx, cancel := detach(ctx)
	defer cancel()
	if err := recordRun(ctx, r.w.Store, r.w.Metrics, r.tenant.Slug, tenantID, runID, jobs.QueueTask, res); err != nil {
		logger.Error("runner run not recorded", "error", err)
	}
	if run != nil {
		r.notes = append(r.notes, run.notes...)
	}
	switch {
	case agentErr != nil:
		return failed(string(ref), agentErr), nil
	case run == nil && errors.Is(cause, errHeartbeatLost):
		return failed(string(ref), errors.New("runner heartbeat lost")), nil
	case run == nil && res.Err != nil:
		return failed(string(ref), res.Err), nil
	case run == nil:
		return failed(string(ref), errors.New("worker: the runner wrote no agent run")), nil
	}
	resp := run.response()
	logger.Info("agent answered", "stop", run.stop, "steps", run.steps, "model", run.model, "input_tokens", resp.InputTokens,
		"cached_tokens", resp.CachedTokens, "output_tokens", resp.OutputTokens, "cost_usd", resp.CostUSD)
	if err := run.stopError(); err != nil {
		return failed(run.model, err), nil
	}
	// The gateway charged every step as it served it; marking the run
	// answered before any write keeps a retry from running it again.
	if err := r.charge(ctx, true); err != nil {
		return failed(run.model, err), nil
	}
	return r.conclude(ctx, run.result, run.model, labels), nil
}

// runnerNotStarted ends a runner run whose runner never started, for
// cause.
func (r *taskRunner) runnerNotStarted(ctx context.Context, runID string, cause error) {
	ctx, cancel := detach(ctx)
	defer cancel()
	err := r.w.Store.WithTenant(ctx, r.tenant.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = 'failed', error = left($2, 2000), finished_at = now() WHERE id = $1`,
			runID, cause.Error())
		return err
	})
	if err != nil {
		r.logger.Warn("runner run not ended", "error", err)
	}
}
