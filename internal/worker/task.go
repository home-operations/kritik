package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/taskrun"
	"github.com/home-operations/kritik/internal/tasks"
)

// Task context bounds: the thread comments a task sees by default, and
// what one comment, one context source and all of them may add to the
// prompt.
const (
	taskThreadComments = 20
	taskCommentBytes   = 8 << 10
	taskSourceBytes    = 32 << 10
	taskContextBytes   = 128 << 10
	taskRelatedMax     = 10
)

// taskSkipAgentic is why an agentic task does not run yet.
const taskSkipAgentic = "agentic tasks arrive in a later release"

// roleTask is the usage role and metric label of a task's model call.
const roleTask = "task"

// Task works the task queue: one job runs one task on one event.
type Task struct {
	river.WorkerDefaults[jobs.TaskArgs]
	Base
	Completers CompleterSource
	configs    repoConfigs
}

// Work implements river.Worker. An error before the model is asked is
// retried, and recorded as the run's failure once the job is out of
// attempts; after it, the run ends whatever happens, since a retry would
// spend the model again and write to the forge twice.
func (w *Task) Work(ctx context.Context, job *river.Job[jobs.TaskArgs]) error {
	args := job.Args
	file := w.Current.Get()
	tenant, err := w.tenant(file, args.TenantID)
	if err != nil {
		return err
	}
	var run store.TaskRun
	var ev store.TaskEvent
	var repo taskRepo
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		if run, err = store.LoadTaskRun(ctx, tx, args.EventID, args.Task); err != nil {
			return err
		}
		if repo, err = loadTaskRepo(ctx, tx, args.RepositoryID); err != nil {
			return err
		}
		ev, err = store.LoadTaskEvent(ctx, tx, args.EventID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrTaskRunGone):
		return river.JobCancel(err)
	case errors.Is(err, store.ErrTaskEventGone):
		return w.finish(ctx, args.TenantID, run.ID, store.TaskRunResult{Status: store.TaskSkipped, Reason: "the event has expired"})
	case err != nil:
		return err
	}
	if run.Status.Terminal() {
		return nil
	}
	logger := w.Logger.With("tenant", tenant.Slug, "repository", repo.name, "task", args.Task, "run", run.ID,
		"subject", ev.SubjectNumber)
	client, err := w.client(ctx, file, repo.installation, repo.externalID, repo.name)
	if err != nil {
		return err
	}
	r := &taskRunner{
		w: w, file: file, tenant: tenant, client: client, args: args, run: run, ev: ev, repo: repo, jobID: job.ID, logger: logger,
	}
	res, err := r.do(ctx)
	if err != nil {
		if job.Attempt < job.MaxAttempts && ctx.Err() == nil {
			return err
		}
		res = store.TaskRunResult{Status: store.TaskFailed, Error: err.Error()}
	}
	ctx, cancel := detach(ctx)
	defer cancel()
	logger.Info("task "+string(res.Status), "reason", res.Reason, "model", res.Model, "error", res.Error)
	return w.finish(ctx, args.TenantID, run.ID, res)
}

func (w *Task) finish(ctx context.Context, tenantID, runID string, res store.TaskRunResult) error {
	if runID == "" {
		return nil
	}
	return w.Store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return store.FinishTaskRun(ctx, tx, runID, res)
	})
}

// taskRunner is one run of one task.
type taskRunner struct {
	w      *Task
	file   *configfile.File
	tenant *configfile.Tenant
	client forge.Client
	args   jobs.TaskArgs
	run    store.TaskRun
	ev     store.TaskEvent
	repo   taskRepo
	jobID  int64
	logger *slog.Logger

	owner, name string
	settings    configfile.Settings
	task        *tasks.Task
	prepared    *tasks.Prepared
	in          tasks.Input
	// headSHA and baseRef are a pull request subject's, as kritik last
	// recorded it; empty for an issue or a pull request it never saw.
	headSHA, baseRef string
}

// skipped ends the run without running it, for reason.
func skipped(reason string) store.TaskRunResult {
	return store.TaskRunResult{Status: store.TaskSkipped, Reason: reason}
}

// failed ends the run as failed, with what the model answered when it did.
func failed(modelName string, err error) store.TaskRunResult {
	return store.TaskRunResult{Status: store.TaskFailed, Model: modelName, Error: err.Error()}
}

// do returns how the run ended, or an error to retry. Only what happens
// before the model is asked is ever retried.
func (r *taskRunner) do(ctx context.Context) (store.TaskRunResult, error) {
	r.owner, r.name = r.repo.split()
	doc, err := r.w.configs.read(ctx, r.client, r.repo.installation, r.owner, r.name, r.args.ConfigSHA)
	if err != nil {
		return store.TaskRunResult{}, err
	}
	eff, _ := effective(r.file.Settings(r.tenant, r.repo.installation, r.repo.name), doc)
	r.settings = eff.Settings
	for i := range eff.Tasks {
		if eff.Tasks[i].Name == r.args.Task {
			r.task = &eff.Tasks[i]
		}
	}
	if r.task == nil {
		return skipped("the task is no longer defined"), nil
	}
	if reason, err := r.rateLimited(ctx); err != nil || reason != "" {
		return skipped(reason), err
	}
	if reason := unsupportedMode(r.task); reason != "" {
		return skipped(reason), nil
	}
	if err := r.w.Store.WithTenant(ctx, r.args.TenantID, func(tx pgx.Tx) error {
		return store.StartTaskRun(ctx, tx, r.run.ID)
	}); err != nil {
		return store.TaskRunResult{}, err
	}
	files, err := r.templateFiles(ctx)
	if err != nil {
		return store.TaskRunResult{}, err
	}
	if r.prepared, err = tasks.Prepare(r.task, files); err != nil {
		return failed("", err), nil
	}
	data, err := r.promptData(ctx)
	if err != nil {
		return store.TaskRunResult{}, err
	}
	labels, err := r.client.RepoLabels(ctx, r.owner, r.name)
	if err != nil {
		return store.TaskRunResult{}, err
	}
	return r.answer(ctx, data, labels), nil
}

// unsupportedMode says why t does not run in this release, or "".
func unsupportedMode(t *tasks.Task) string {
	if t.RunMode() != tasks.ModeSingle {
		return taskSkipAgentic
	}
	return ""
}

// rateLimited says why the run is skipped when the task has run on its
// subject as often as the operator allows in the last hour, or "".
func (r *taskRunner) rateLimited(ctx context.Context) (string, error) {
	limit := r.settings.TaskBounds.MaxRunsPerSubjectPerHour
	if r.ev.SubjectNumber == 0 || limit <= 0 {
		return "", nil
	}
	var n int
	err := r.w.Store.WithTenant(ctx, r.args.TenantID, func(tx pgx.Tx) error {
		var err error
		n, err = store.CountTaskRuns(ctx, tx, r.args.RepositoryID, r.args.Task, r.ev.SubjectNumber, time.Hour, r.run.ID)
		return err
	})
	if err != nil {
		return "", err
	}
	return rateLimitReason(n, limit), nil
}

// rateLimitReason is why a run is skipped after n runs in the last hour
// against a limit, or "".
func rateLimitReason(n, limit int) string {
	if limit <= 0 || n < limit {
		return ""
	}
	return fmt.Sprintf("maxRunsPerSubjectPerHour (%d) reached", limit)
}

// templateFiles reads the files the task's templates name at the commit
// its definition came from. A missing one is left out, for Prepare to
// report.
func (r *taskRunner) templateFiles(ctx context.Context) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, p := range r.task.Files() {
		b, err := r.client.FileAt(ctx, r.owner, r.name, r.args.ConfigSHA, p)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, forge.ErrFileTooLarge):
			continue
		case err != nil:
			return nil, err
		}
		files[p] = b
	}
	return files, nil
}

// promptData gathers the subject as it is now, and the context sources the
// task names.
func (r *taskRunner) promptData(ctx context.Context) (tasks.PromptData, error) {
	r.in = taskInput(r.ev, r.repo, r.repo.defaultBranch)
	d := tasks.PromptData{Context: map[string]any{}, Task: r.task}
	if r.ev.SubjectNumber > 0 {
		issue, err := r.client.Issue(ctx, r.owner, r.name, r.ev.SubjectNumber)
		if err != nil {
			return d, err
		}
		r.in.Subject = taskrun.Subject(issue)
		if issue.IsPull {
			if err := r.pullHead(ctx); err != nil {
				return d, err
			}
		}
		if th := r.task.Context.Thread; th != nil {
			comments, err := r.client.ListConversation(ctx, r.owner, r.name, r.ev.SubjectNumber)
			if err != nil {
				return d, err
			}
			d.Thread = threadTail(comments, th.Comments)
		}
	}
	d.Input = r.in
	budget := taskContextBytes
	add := func(name, value string) {
		value = clipBytes(value, min(taskSourceBytes, budget))
		budget -= len(value)
		d.Context[name] = value
	}
	for _, f := range r.task.Context.Files {
		if f.Path == "" {
			r.logger.Debug("task context glob not gathered in single mode", "glob", f.Glob)
			continue
		}
		b, err := r.client.FileAt(ctx, r.owner, r.name, r.args.ConfigSHA, f.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, forge.ErrFileTooLarge):
			continue
		case err != nil:
			return d, err
		}
		add(f.Path, string(b))
	}
	queries, err := r.prepared.Queries(r.in)
	if err != nil {
		return d, err
	}
	for _, q := range queries {
		if q.Kind != tasks.ContextRelated {
			r.logger.Debug("task index search not gathered in single mode", "query", q.Name)
			continue
		}
		found, err := r.client.SearchIssues(ctx, r.owner, r.name, q.Query, min(max(q.K, 1), taskRelatedMax))
		if err != nil {
			return d, err
		}
		related, err := json.Marshal(relatedIssues(found, r.ev.SubjectNumber))
		if err != nil {
			return d, fmt.Errorf("worker: encode related issues: %w", err)
		}
		add(q.Name, string(related))
	}
	return d, nil
}

// pullHead reads the head and base a pull request subject was last
// recorded with, which inline comments are pinned to.
func (r *taskRunner) pullHead(ctx context.Context) error {
	err := r.w.Store.WithTenant(ctx, r.args.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT head_sha, base_ref FROM pull_requests WHERE repository_id = $1 AND number = $2`,
			r.args.RepositoryID, r.ev.SubjectNumber).Scan(&r.headSHA, &r.baseRef)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: read pull request head: %w", err)
	}
	return nil
}

// relatedIssue is one search result as a task's prompt sees it.
type relatedIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Pull   bool     `json:"pull"`
	Labels []string `json:"labels,omitempty"`
	URL    string   `json:"url"`
}

// relatedIssues are found without the subject itself.
func relatedIssues(found []forge.Issue, subject int) []relatedIssue {
	out := []relatedIssue{}
	for _, i := range found {
		if i.Number != subject {
			out = append(out, relatedIssue{Number: i.Number, Title: i.Title, State: i.State, Pull: i.IsPull, Labels: i.Labels, URL: i.URL})
		}
	}
	return out
}

// threadTail is the last n comments of a thread, oldest first, each cut to
// taskCommentBytes; n of zero or less is taskThreadComments.
func threadTail(comments []forge.Comment, n int) []tasks.Comment {
	if n <= 0 {
		n = taskThreadComments
	}
	if len(comments) > n {
		comments = comments[len(comments)-n:]
	}
	out := make([]tasks.Comment, len(comments))
	for i, c := range comments {
		out[i] = tasks.Comment{Author: c.Author, Body: clipBytes(c.Body, taskCommentBytes), CreatedAt: c.CreatedAt}
	}
	return out
}

// clipBytes cuts s to at most n bytes, on a rune boundary.
func clipBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// answer asks the model, plans its answer and applies the plan. The model's
// tokens are spent once it answers, so from then on the run ends here
// whatever the job's ctx does.
func (r *taskRunner) answer(ctx context.Context, data tasks.PromptData, labels []string) store.TaskRunResult {
	ref := configfile.ModelRef(r.task.Models.Review)
	if ref == "" {
		ref = r.settings.Models.Review
	}
	if ref == "" {
		return skipped("no review model is configured for this repository")
	}
	fallback := configfile.ModelRef(r.task.Models.Fallback)
	if fallback == "" {
		fallback = r.settings.Models.Fallback
	}
	system, user, err := r.prepared.RenderPrompt(data)
	if err != nil {
		return failed("", err)
	}
	schema, err := json.Marshal(r.prepared.AnswerSchema(labels))
	if err != nil {
		return failed("", fmt.Errorf("worker: encode answer schema: %w", err))
	}
	req := model.CompletionRequest{
		System: system, User: user, Model: ref.Model(), Schema: schema, SchemaName: "task_answer", MaxTokens: maxOutputTokens,
	}
	resp, err := r.complete(ctx, ref, fallback, req)
	if capped, ok := errors.AsType[cappedError](err); ok {
		return skipped(string(capped))
	}
	if err != nil {
		return failed(string(ref), err)
	}
	ctx, cancel := detach(ctx)
	defer cancel()
	r.recordUsage(ctx, resp)
	answer, err := r.prepared.ParseAnswer([]byte(resp.Raw))
	if err != nil {
		return failed(resp.Model, err)
	}
	pl, err := r.prepared.Plan(r.in, answer, tasks.Facts{RepoLabels: labels, Subject: r.in.Subject, UserAllowed: r.userAllowed(ctx)})
	if err != nil {
		return failed(resp.Model, err)
	}
	r.anchor(ctx, &pl)
	return r.apply(ctx, answer, pl, resp.Model)
}

// complete asks the model under a lease on it, once the tenant's monthly
// token cap allows, falling back as a review does: a fallback on the same
// provider is the provider's to try, one on another is a second call.
func (r *taskRunner) complete(
	ctx context.Context, ref, fallback configfile.ModelRef, req model.CompletionRequest,
) (model.CompletionResponse, error) {
	if fallback != "" && fallback.Provider() == ref.Provider() {
		req.Fallbacks = []string{fallback.Model()}
	}
	var resp model.CompletionResponse
	err := r.w.withLease(ctx, r.tenant, string(ref), r.settings.Limits.Concurrency, r.jobID, func(ctx context.Context) error {
		limits := r.settings.Limits
		limits.ReviewsPerDay = 0
		capped, err := capReached(ctx, r.w.Store, r.tenant.ID(), limits)
		if err != nil {
			return err
		}
		if capped != "" {
			return cappedError(capped)
		}
		resp, err = r.call(ctx, ref, req, 0)
		if err == nil || fallback == "" || fallback.Provider() == ref.Provider() || ctx.Err() != nil {
			return err
		}
		r.logger.Warn("primary model failed, trying fallback", "model", ref, "fallback", fallback, "error", err)
		req.Model, req.Fallbacks = fallback.Model(), nil
		var ferr error
		if resp, ferr = r.call(ctx, fallback, req, 1); ferr != nil {
			return errors.Join(err, ferr)
		}
		return nil
	})
	return resp, err
}

// call makes one structured call on ref's provider, recorded as step.
func (r *taskRunner) call(
	ctx context.Context, ref configfile.ModelRef, req model.CompletionRequest, step int,
) (model.CompletionResponse, error) {
	stepper, err := r.w.Completers.Stepper(r.file, ref.Provider())
	if err != nil {
		return model.CompletionResponse{}, err
	}
	c := store.ModelCall{TenantID: r.tenant.ID(), TaskRunID: r.run.ID, Kind: store.ModelCallTask, Step: step}
	mask := transcriptMask(r.file, r.file.Providers[ref.Provider()])
	completer := model.Structured{Stepper: stepper, OnStep: r.w.onStep(ctx, r.logger, c, mask)}
	resp, err := completer.Complete(ctx, req)
	r.w.Metrics.ModelCall(r.tenant.Slug, string(ref), roleTask, callOutcome(err), resp.InputTokens, resp.CachedTokens, resp.OutputTokens,
		resp.CostUSD)
	return resp, err
}

// recordUsage charges the call to the tenant, where its caps count it.
func (r *taskRunner) recordUsage(ctx context.Context, resp model.CompletionResponse) {
	err := r.w.Store.WithTenant(ctx, r.tenant.ID(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO usage (tenant_id, repository_id, role, model, upstream, input_tokens, output_tokens, cost_usd)
			VALUES ($1, $2, 'task', $3, $4, $5, $6, $7)`,
			r.tenant.ID(), r.args.RepositoryID, resp.Model, resp.Upstream, resp.InputTokens, resp.OutputTokens, resp.CostUSD); err != nil {
			return fmt.Errorf("worker: record task usage: %w", err)
		}
		return nil
	})
	if err != nil {
		r.logger.Error("task usage not recorded", "error", err)
	}
}

// userAllowed lets a login be assigned or asked for a review when it has at
// least read access to the repository.
func (r *taskRunner) userAllowed(ctx context.Context) func(string) bool {
	return func(login string) bool {
		perm, err := r.client.Permission(ctx, r.owner, r.name, login)
		if err != nil {
			r.logger.Warn("permission lookup failed", "login", login, "error", err)
			return false
		}
		return perm.Valid() && perm != forge.PermissionNone
	}
}

// anchor keeps the plan's inline comments the pull request's diff shows.
func (r *taskRunner) anchor(ctx context.Context, pl *tasks.Plan) {
	if len(pl.Inline) == 0 {
		return
	}
	if r.headSHA == "" {
		taskrun.DropInline(pl, "kritik has not recorded the pull request's head")
		return
	}
	base, err := r.client.MergeBase(ctx, r.owner, r.name, r.ev.SubjectNumber, r.baseRef, r.headSHA)
	if err != nil {
		taskrun.DropInline(pl, "forge: "+err.Error())
		return
	}
	diff, err := r.client.PullRequestDiff(ctx, r.owner, r.name, r.ev.SubjectNumber, base, r.headSHA)
	if err != nil {
		taskrun.DropInline(pl, "forge: "+err.Error())
		return
	}
	kept, dropped := taskrun.Anchor(pl.Inline, review.Anchors(diff))
	pl.Inline, pl.Dropped = kept, append(pl.Dropped, dropped...)
}

// taskApplied is what a run did, as task_runs.applied records it.
type taskApplied struct {
	AddLabels    []string       `json:"add_labels,omitempty"`
	RemoveLabels []string       `json:"remove_labels,omitempty"`
	Assignees    []string       `json:"assignees,omitempty"`
	Reviewers    []string       `json:"reviewers,omitempty"`
	State        string         `json:"state,omitempty"`
	Inline       []tasks.Inline `json:"inline,omitempty"`
	// Comment is the report comment's mode, when it was posted.
	Comment string `json:"comment,omitempty"`
}

// taskDrop is an action a run left out, as task_runs.dropped records it.
type taskDrop struct {
	Action string `json:"action"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

func taskDrops(ds []tasks.Drop) []taskDrop {
	out := make([]taskDrop, len(ds))
	for i, d := range ds {
		out[i] = taskDrop(d)
	}
	return out
}

// apply makes the plan's writes, then posts the report comment rendered
// with what the forge accepted. The run fails only when every write it
// tried failed.
func (r *taskRunner) apply(ctx context.Context, answer tasks.Answer, pl tasks.Plan, modelName string) store.TaskRunResult {
	t := taskrun.Target{Owner: r.owner, Repo: r.name, Number: r.ev.SubjectNumber, HeadSHA: r.headSHA}
	res := taskrun.Apply(ctx, r.client, t, pl)
	dropped := append(pl.Dropped, res.Dropped...)
	a := res.Applied
	applied := taskApplied{
		AddLabels: a.AddLabels, RemoveLabels: a.RemoveLabels, Assignees: a.Assignees, Reviewers: a.Reviewers, State: a.State, Inline: a.Inline,
	}
	attempted, refused := res.Attempted, len(res.Dropped)
	var commentID int64
	if pl.Comment != nil {
		body, err := r.prepared.RenderComment(tasks.OutputData{Input: r.in, Task: r.task, Answer: answer, Applied: res.Applied, Dropped: dropped})
		if err != nil {
			r.logger.Warn("report comment re-render failed", "error", err)
			body = pl.Comment.Body
		}
		if body != "" {
			attempted++
			login, err := r.client.BotLogin(ctx)
			if err == nil {
				commentID, err = taskrun.Post(ctx, r.client, t, r.task.Name, login, pl.Comment.Mode, body)
			}
			if err != nil {
				refused++
				dropped = append(dropped, taskrun.CommentDrop(pl.Comment.Mode, err))
			} else {
				applied.Comment = pl.Comment.Mode
			}
		}
	}
	out := store.TaskRunResult{Status: store.TaskSucceeded, Model: modelName, CommentID: commentID}
	if attempted > 0 && refused == attempted {
		out.Status, out.Error = store.TaskFailed, "the forge refused every write"
	}
	out.Fields = marshalOrNil(r.logger, "fields", answer.Fields)
	out.Proposed = marshalOrNil(r.logger, "proposed", answer)
	out.Applied = marshalOrNil(r.logger, "applied", applied)
	out.Dropped = marshalOrNil(r.logger, "dropped", taskDrops(dropped))
	return out
}

// marshalOrNil encodes v for a jsonb column, nil (NULL) when it cannot be.
func marshalOrNil(logger *slog.Logger, what string, v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		logger.Warn("task run record not encoded", "what", what, "error", err)
		return nil
	}
	return b
}
