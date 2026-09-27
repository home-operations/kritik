package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/taskrun"
	"github.com/home-operations/kritik/internal/tasks"
)

// taskRepo is the repository a task event concerns, as the store has it.
type taskRepo struct {
	id, name, defaultBranch, installation string
	externalID                            int64
	// configSHA is the default branch tip whose .kritik.yaml a dispatch
	// last recorded, "" before the first.
	configSHA string
}

func (r taskRepo) split() (owner, name string) {
	owner, name, _ = strings.Cut(r.name, "/")
	return owner, name
}

func loadTaskRepo(ctx context.Context, tx pgx.Tx, id string) (taskRepo, error) {
	r := taskRepo{id: id}
	err := tx.QueryRow(ctx, `SELECT r.name, r.default_branch, i.name, coalesce(i.external_id, 0), r.task_config_sha
		FROM repositories r JOIN installations i ON i.id = r.installation_id WHERE r.id = $1`, id).
		Scan(&r.name, &r.defaultBranch, &r.installation, &r.externalID, &r.configSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, river.JobCancel(fmt.Errorf("worker: repository %s is unknown", id))
	}
	if err != nil {
		return r, fmt.Errorf("worker: load repository: %w", err)
	}
	return r, nil
}

// repoConfigs caches .kritik.yaml by repository and commit: a commit's file
// never changes, and every event on a quiet default branch reads the same
// one.
type repoConfigs struct {
	mu   sync.Mutex
	docs map[string][]byte
}

// repoConfigsMax bounds the cache; past it, it starts over.
const repoConfigsMax = 512

// read returns the .kritik.yaml of owner/repo at sha, nil when it has none
// or one too large to use.
func (c *repoConfigs) read(ctx context.Context, client forge.Client, installation, owner, repo, sha string) ([]byte, error) {
	key := installation + "\x00" + owner + "/" + repo + "\x00" + sha
	c.mu.Lock()
	doc, ok := c.docs[key]
	c.mu.Unlock()
	if ok {
		return doc, nil
	}
	doc, _, err := readRepoConfig(ctx, client, owner, repo, sha)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.docs == nil || len(c.docs) >= repoConfigsMax {
		c.docs = map[string][]byte{}
	}
	c.docs[key] = doc
	return doc, nil
}

// taskInput is the stored event as a task sees it, without its subject,
// which the caller reads fresh from the forge.
func taskInput(ev store.TaskEvent, repo taskRepo, defaultBranch string) tasks.Input {
	r := taskrun.Repo(repo.name, defaultBranch)
	return tasks.Input{
		Forge: ev.Forge, Event: ev.Event, RawEvent: ev.RawEvent, Action: ev.Action, Sender: ev.Sender,
		Raw: taskrun.DecodeRaw(ev.Payload), Repo: r,
	}
}

// matchTasks are the names of the tasks of ts in fires. A task whose guard
// fails to evaluate is left out and logged: its author's mistake must not
// stop the others.
func matchTasks(ts []tasks.Task, in tasks.Input, logger *slog.Logger) []tasks.Task {
	var out []tasks.Task
	for _, t := range ts {
		ok, err := t.Matches(in)
		if err != nil {
			logger.Warn("task guard failed", "task", t.Name, "error", err)
			continue
		}
		if ok {
			out = append(out, t)
		}
	}
	return out
}

// isBot reports whether sender is the installation's own bot, whose events
// never trigger a task: its label and comment writes would re-trigger the
// task that made them.
func isBot(sender, botLogin string) bool {
	return sender != "" && strings.EqualFold(sender, botLogin)
}

// TaskDispatch works task_dispatch jobs: it resolves the tasks a stored
// delivery runs, from the operator's settings and the repository's
// .kritik.yaml at the default branch tip, and enqueues one task job for
// each that matches.
type TaskDispatch struct {
	river.WorkerDefaults[jobs.TaskDispatchArgs]
	Base
	configs repoConfigs
}

// Work implements river.Worker.
func (w *TaskDispatch) Work(ctx context.Context, job *river.Job[jobs.TaskDispatchArgs]) error {
	args := job.Args
	file := w.Current.Get()
	tenant, err := w.tenant(file, args.TenantID)
	if err != nil {
		return err
	}
	var ev store.TaskEvent
	var repo taskRepo
	err = w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		if ev, err = store.LoadTaskEvent(ctx, tx, args.EventID); err != nil {
			return err
		}
		repo, err = loadTaskRepo(ctx, tx, args.RepositoryID)
		return err
	})
	if errors.Is(err, store.ErrTaskEventGone) {
		return nil
	}
	if err != nil {
		return err
	}
	logger := w.Logger.With("tenant", tenant.Slug, "repository", repo.name, "event", ev.RawEvent, "action", ev.Action,
		"delivery", ev.Delivery)
	client, err := w.client(ctx, file, repo.installation, repo.externalID, repo.name)
	if err != nil {
		return err
	}
	login, err := client.BotLogin(ctx)
	if err != nil {
		return err
	}
	if isBot(ev.Sender, login) {
		logger.Debug("task event from the bot itself ignored")
		return w.dropEvent(ctx, args)
	}
	owner, name := repo.split()
	sha, branch, err := client.BranchTip(ctx, owner, name, "")
	if err != nil {
		return err
	}
	doc, err := w.configs.read(ctx, client, repo.installation, owner, name, sha)
	if err != nil {
		return err
	}
	if sha != repo.configSHA {
		if err := w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
			return store.SaveRepoTaskConfig(ctx, tx, repo.id, sha, doc)
		}); err != nil {
			// The record only feeds the dashboard's task list; the
			// dispatch does not depend on it.
			logger.Warn("repository task config not recorded", "error", err)
		}
	}
	eff, err := repoconfig.Merge(doc, file.Settings(tenant, repo.installation, repo.name))
	if err != nil {
		logger.Warn("repository .kritik.yaml ignored; only the operator's tasks run", "config_sha", short(sha), "error", err)
	}
	for _, n := range eff.Dropped {
		logger.Debug("repository configuration note", "note", n)
	}
	for _, n := range eff.TaskNotes {
		logger.Debug("task left out", "note", n.String())
	}
	in := taskInput(ev, repo, branch)
	if ev.SubjectNumber > 0 && len(eff.Tasks) > 0 {
		issue, err := client.Issue(ctx, owner, name, ev.SubjectNumber)
		if err != nil {
			return err
		}
		in.Subject = taskrun.Subject(issue, in.Raw)
	}
	matched := matchTasks(eff.Tasks, in, logger)
	if len(matched) == 0 {
		logger.Debug("no task matches the event")
		return w.dropEvent(ctx, args)
	}
	runs, queued := taskJobs(matched, args, ev, taskrun.Names(in), sha)
	queue := river.ClientFromContext[pgx.Tx](ctx)
	return w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		for i, run := range runs {
			inserted, err := store.QueueTaskRun(ctx, tx, run)
			if err != nil {
				return err
			}
			if !inserted {
				continue
			}
			if _, err := queue.InsertTx(ctx, tx, queued[i], nil); err != nil {
				return fmt.Errorf("worker: enqueue task: %w", err)
			}
			logger.Info("task queued", "task", run.Task, "mode", run.Mode, "config_sha", short(sha))
		}
		return nil
	})
}

// taskJobs are the queued run and the job of each matched task on the
// event args dispatches, whose event names are names, with the tasks as
// defined at sha.
func taskJobs(
	matched []tasks.Task, args jobs.TaskDispatchArgs, ev store.TaskEvent, names []string, sha string,
) ([]store.TaskRun, []jobs.TaskArgs) {
	var trigger string
	if len(names) > 0 {
		trigger = names[0]
	}
	runs := make([]store.TaskRun, len(matched))
	queued := make([]jobs.TaskArgs, len(matched))
	for i, t := range matched {
		runs[i] = store.TaskRun{
			TenantID: args.TenantID, RepositoryID: args.RepositoryID, Task: t.Name, EventID: args.EventID,
			SubjectKind: ev.SubjectKind, SubjectNumber: ev.SubjectNumber, Trigger: trigger, Mode: string(t.RunMode()), ConfigSHA: sha,
		}
		queued[i] = jobs.TaskArgs{TenantID: args.TenantID, RepositoryID: args.RepositoryID, EventID: args.EventID, Task: t.Name, ConfigSHA: sha}
	}
	return runs, queued
}

// dropEvent deletes a task event no task runs on.
func (w *TaskDispatch) dropEvent(ctx context.Context, args jobs.TaskDispatchArgs) error {
	return w.Store.WithTenant(ctx, args.TenantID, func(tx pgx.Tx) error {
		return store.DeleteTaskEvent(ctx, tx, args.EventID)
	})
}
