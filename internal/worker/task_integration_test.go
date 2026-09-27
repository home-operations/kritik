//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
	"github.com/home-operations/kritik/internal/webhook"
)

const taskConfigYAML = `
providers:
  test:
    type: openai
    baseUrl: http://unused.invalid/v1
    apiKey: { env: TEST_SECRET }
defaults:
  models:
    review: test/reviewer
  limits:
    concurrency: 1
  allow:
    tasks: { enabled: true }
  tasks:
    - name: triage
      mode: single
      on: [{ issue: [opened] }]
      promptInline: "Triage issue #{{ .Subject.Number }}: {{ .Subject.Title }}"
      context: { thread: { comments: 5 } }
      fields:
        priority: { type: string, enum: [low, high] }
      actions:
        labels: { propose: { add: [bug, enhancement], remove: [needs-triage] } }
        comment: { mode: sticky }
tenants:
  - slug: onedr0p
    installations:
      - name: bot-ross
        forge: github
        account: onedr0p
        app:
          clientId: Iv1.x
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
    repositories:
      - name: onedr0p/home-ops
`

// taskForge is a forge holding one issue, its labels and its comments.
type taskForge struct {
	forge.Client

	mu       sync.Mutex
	labels   []string
	comments map[int64]string
	creates  int
}

func (f *taskForge) BotLogin(context.Context) (string, error) { return "kritik[bot]", nil }

func (f *taskForge) BranchTip(context.Context, string, string, string) (string, string, error) {
	return "c0ffee", "main", nil
}

func (f *taskForge) FileAt(_ context.Context, _, _, _, path string) ([]byte, error) {
	return nil, fmt.Errorf("task forge: %s: %w", path, fs.ErrNotExist)
}

func (f *taskForge) Issue(_ context.Context, _, _ string, number int) (forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return forge.Issue{Number: number, Title: "It crashes", Body: "on start", State: "open", Author: "devin", Labels: slices.Clone(f.labels)}, nil
}

func (f *taskForge) RepoLabels(context.Context, string, string) ([]string, error) {
	return []string{"bug", "enhancement", "needs-triage"}, nil
}

func (f *taskForge) AddLabels(_ context.Context, _, _ string, _ int, ls []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range ls {
		if !slices.Contains(f.labels, l) {
			f.labels = append(f.labels, l)
		}
	}
	return nil
}

func (f *taskForge) RemoveLabel(_ context.Context, _, _ string, _ int, l string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = slices.DeleteFunc(f.labels, func(x string) bool { return x == l })
	return nil
}

func (f *taskForge) ListConversation(context.Context, string, string, int) ([]forge.Comment, error) {
	return []forge.Comment{{ID: 1, Author: "devin", Body: "Stack trace attached.", CreatedAt: time.Unix(1, 0)}}, nil
}

func (f *taskForge) FindComment(_ context.Context, _, _ string, _ int, _, marker string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, body := range f.comments {
		if strings.Contains(body, marker) {
			return id, nil
		}
	}
	return 0, nil
}

func (f *taskForge) CreateComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[int64]string{}
	}
	f.creates++
	id := int64(len(f.comments) + 1)
	f.comments[id] = body
	return id, nil
}

func (f *taskForge) UpdateComment(_ context.Context, _, _ string, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[id] = body
	return nil
}

func (f *taskForge) state() ([]string, map[int64]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.labels), maps.Clone(f.comments), f.creates
}

// taskModel answers every task with a bug of high priority.
// With block set it holds every call until the call's context ends.
type taskModel struct {
	mu    sync.Mutex
	users []string
	block atomic.Bool
}

func (m *taskModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users)
}

func (m *taskModel) Step(ctx context.Context, req model.StepRequest) (model.StepResponse, error) {
	m.mu.Lock()
	m.users = append(m.users, req.Messages[0].Text)
	m.mu.Unlock()
	if m.block.Load() {
		<-ctx.Done()
		return model.StepResponse{}, ctx.Err()
	}
	return model.StepResponse{
		ToolCalls: []model.ToolCall{{ID: "call", Name: req.Tools[0].Name, Input: json.RawMessage(
			`{"summary":"A crash on start.","fields":{"priority":"high"},"labels":{"add":["bug"],"remove":["needs-triage"]},"comment":""}`)}},
		Stop: model.StopToolUse, Usage: model.Usage{Input: 30, Output: 10}, Model: req.Model,
	}, nil
}

// failingForges fails every client it is asked for while fail is set.
type failingForges struct {
	forges
	fail atomic.Bool
}

func (f *failingForges) For(ctx context.Context, in *configfile.Installation, id int64, repo string) (forge.Client, error) {
	if f.fail.Load() {
		return nil, errors.New("forge down")
	}
	return f.forges.For(ctx, in, id, repo)
}

// taskHarness runs the task queue over a store, a taskForge and a
// taskModel, and dispatches issue events through the real ingest service.
// The task worker's forges fail on demand; its runs are those created
// since the harness started.
type taskHarness struct {
	t      *testing.T
	client *river.Client[pgx.Tx]
	taskFg *failingForges
	since  time.Time
	st     *store.Store
	file   *configfile.File
	svc    *ingest.Service
	tenant *configfile.Tenant
	in     *configfile.Installation
	tf     *taskForge
	tm     *taskModel
}

func newTaskHarness(t *testing.T, timeout time.Duration) *taskHarness {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.Options{AppURL: env(t, "KRITIK_TEST_APP_URL"), OwnerURL: env(t, "KRITIK_TEST_OWNER_URL"), Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "test-provider-key")
	file, err := configfile.Parse([]byte(taskConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatal(err)
	}
	insertOnly, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	h := &taskHarness{t: t, st: st, file: file, svc: ingest.NewService(st, insertOnly), tf: &taskForge{labels: []string{"needs-triage"}},
		tm: &taskModel{}}
	h.in, h.tenant, _ = file.Installation("bot-ross")
	workers := river.NewWorkers()
	base := Base{Store: st, Current: configfile.NewCurrent(file), Forges: &forges{f: h.tf}, Logger: logger}
	river.AddWorker(workers, &TaskDispatch{Base: base})
	h.taskFg = &failingForges{}
	h.taskFg.f = h.tf
	taskBase := base
	taskBase.Forges = h.taskFg
	river.AddWorker(workers, &Task{Base: taskBase, Completers: &completers{c: h.tm}, timeout: timeout})
	client, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{
		Queues: map[string]river.QueueConfig{jobs.QueueTask: {MaxWorkers: 2}}, Workers: workers,
		FetchCooldown: 50 * time.Millisecond, FetchPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background()) })
	h.client = client
	if err := st.App().QueryRow(ctx, `SELECT now()`).Scan(&h.since); err != nil {
		t.Fatal(err)
	}
	return h
}

// dispatch delivers issue #7's opening as delivery, sent by sender.
func (h *taskHarness) dispatch(delivery, sender string) ingest.Outcome {
	h.t.Helper()
	out, err := h.svc.Dispatch(context.Background(), ingest.Request{File: h.file, Tenant: h.tenant, Installation: h.in, Event: webhook.Event{
		Kind: webhook.KindIssue, Action: "opened", Delivery: delivery, Account: "onedr0p", Forge: configfile.ForgeGitHub,
		RawEvent: "issues", Sender: sender, Raw: json.RawMessage(`{"action":"opened","issue":{"number":7}}`),
		Repository: &webhook.Repository{FullName: "onedr0p/home-ops", DefaultBranch: "main"},
		Subject:    &webhook.Subject{Kind: webhook.SubjectIssue, Number: 7},
		Issue:      &webhook.Issue{Number: 7, Title: "It crashes", State: "open", Author: "devin"},
	}})
	if err != nil {
		h.t.Fatalf("dispatch: %v", err)
	}
	return out
}

type taskRunRow struct {
	status, reason, model, errText string
	fields, applied, dropped       []byte
	commentID                      *int64
}

// runs are the triage task's runs, oldest first.
func (h *taskHarness) runs() []taskRunRow {
	h.t.Helper()
	ctx := context.Background()
	var out []taskRunRow
	err := h.st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT status, reason, model, error, coalesce(fields, 'null'), coalesce(applied, 'null'),
			coalesce(dropped, 'null'), comment_id FROM task_runs WHERE task = 'triage' AND created_at >= $1 ORDER BY created_at`, h.since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r taskRunRow
			if err := rows.Scan(&r.status, &r.reason, &r.model, &r.errText, &r.fields, &r.applied, &r.dropped, &r.commentID); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// waitRuns waits for the nth run to end and returns it.
func (h *taskHarness) waitRuns(n int) taskRunRow {
	h.t.Helper()
	waitFor(h.t, 20*time.Second, fmt.Sprintf("task run %d", n), func() bool {
		rs := h.runs()
		return len(rs) == n && store.TaskRunStatus(rs[n-1].status).Terminal()
	})
	return h.runs()[n-1]
}

// count counts the rows query finds in the tenant.
func (h *taskHarness) count(query string) int {
	h.t.Helper()
	ctx := context.Background()
	var n int
	if err := h.st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error { return tx.QueryRow(ctx, query).Scan(&n) }); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestTaskEndToEnd(t *testing.T) {
	h := newTaskHarness(t, 0)
	if out := h.dispatch("d-1", "devin"); out.Status != ingest.Enqueued || out.Job != "task_dispatch" {
		t.Fatalf("an issue a task runs on = %+v", out)
	}
	first := h.waitRuns(1)
	checkFirstTaskRun(t, h, first)

	// A redelivery is not run again.
	if out := h.dispatch("d-1", "devin"); out.Status == ingest.Enqueued {
		t.Fatalf("a redelivery = %+v", out)
	}
	// kritik's own event never triggers a task, and leaves no event behind.
	if out := h.dispatch("d-bot", "kritik[bot]"); out.Status != ingest.Enqueued {
		t.Fatalf("the bot's event = %+v", out)
	}
	waitFor(t, 20*time.Second, "the bot's event to be dropped", func() bool {
		return h.count(`SELECT count(*) FROM task_events WHERE delivery = 'd-bot'`) == 0
	})
	if n := len(h.runs()); n != 1 {
		t.Fatalf("the bot's event ran %d task(s)", n-1)
	}

	// A second event updates the sticky comment in place.
	h.dispatch("d-2", "devin")
	second := h.waitRuns(2)
	if second.status != "succeeded" || second.commentID == nil || *second.commentID != *first.commentID {
		t.Fatalf("second run = %+v", second)
	}
	if _, comments, creates := h.tf.state(); creates != 1 || len(comments) != 1 {
		t.Fatalf("the sticky comment was not updated in place: %d creates, %v", creates, comments)
	}
	if usage, calls := h.count(`SELECT count(*) FROM usage WHERE role = 'task'`),
		h.count(`SELECT count(*) FROM model_calls WHERE kind = 'task' AND task_run_id IS NOT NULL`); usage != 2 || calls != 2 {
		t.Fatalf("usage rows = %d, model calls = %d; want 2 each", usage, calls)
	}

	checkRetryAfterAnswer(t, h)
	checkFinalAttemptFails(t, h)

	// Retention deletes the events; the runs keep their record.
	if n, err := h.st.SweepTaskEvents(context.Background(), time.Nanosecond); err != nil || n != 3 {
		t.Fatalf("SweepTaskEvents = %d, %v; want the three runs' events", n, err)
	}
	if n := h.count(`SELECT count(*) FROM task_runs WHERE event_id IS NULL`); n != 3 {
		t.Fatalf("runs without their event = %d", n)
	}
}

// checkFirstTaskRun checks the first triage run's record, what it wrote to
// the forge, and what its prompt held.
func checkFirstTaskRun(t *testing.T, h *taskHarness, first taskRunRow) {
	t.Helper()
	var fields, applied map[string]any
	if err := json.Unmarshal(first.fields, &fields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(first.applied, &applied); err != nil {
		t.Fatal(err)
	}
	if first.status != "succeeded" || first.model != "reviewer" || fields["priority"] != "high" || first.commentID == nil ||
		fmt.Sprint(applied) != "map[add_labels:[bug] comment:sticky remove_labels:[needs-triage]]" || string(first.dropped) != "[]" {
		t.Fatalf("first run = %+v (fields %s, applied %s, dropped %s)", first, first.fields, first.applied, first.dropped)
	}
	labels, comments, creates := h.tf.state()
	body := comments[*first.commentID]
	if !slices.Equal(labels, []string{"bug"}) || creates != 1 || !strings.Contains(body, tasks.StickyMarker("triage")) ||
		!strings.Contains(body, "A crash on start.") {
		t.Fatalf("forge after the first run: labels %q, comments %v, creates %d", labels, comments, creates)
	}
	h.tm.mu.Lock()
	prompt := h.tm.users[0]
	h.tm.mu.Unlock()
	if !strings.Contains(prompt, "Triage issue #7: It crashes") || !strings.Contains(prompt, "Stack trace attached.") {
		t.Fatalf("prompt = %q", prompt)
	}
}

// latestTaskJob is the id of the newest task job.
func (h *taskHarness) latestTaskJob() int64 {
	h.t.Helper()
	var id int64
	if err := h.st.App().QueryRow(context.Background(), `SELECT id FROM river_job WHERE kind = 'task' ORDER BY id DESC LIMIT 1`).
		Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// checkRetryAfterAnswer retries the job of a run whose model had answered
// as if its record had never landed: the retry ends the run, without asking
// the model again, charging it again or writing to the forge again.
func checkRetryAfterAnswer(t *testing.T, h *taskHarness) {
	t.Helper()
	ctx := context.Background()
	calls, usage := h.tm.calls(), h.count(`SELECT count(*) FROM usage WHERE role = 'task'`)
	labels, comments, creates := h.tf.state()
	if err := h.st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE task_runs SET status = 'running', finished_at = NULL
			WHERE id = (SELECT id FROM task_runs WHERE answered_at IS NOT NULL ORDER BY created_at DESC LIMIT 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	job := h.latestTaskJob()
	// A job still running when it is retried is left alone.
	waitFor(t, 20*time.Second, "the run's job to complete", func() bool {
		var state string
		_ = h.st.App().QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`, job).Scan(&state)
		return state == "completed"
	})
	if _, err := h.client.JobRetry(ctx, job); err != nil {
		t.Fatal(err)
	}
	last := h.waitRuns(2)
	if last.status != "failed" || last.errText != taskInterrupted {
		t.Fatalf("a retried answered run = %+v", last)
	}
	l2, c2, cr2 := h.tf.state()
	if h.tm.calls() != calls || h.count(`SELECT count(*) FROM usage WHERE role = 'task'`) != usage ||
		!slices.Equal(labels, l2) || !maps.Equal(comments, c2) || creates != cr2 {
		t.Fatalf("the retry repeated the model call or a write: calls %d→%d, labels %q→%q, creates %d→%d",
			calls, h.tm.calls(), labels, l2, creates, cr2)
	}
}

// checkFinalAttemptFails runs a job whose forge client cannot be built on
// its only attempt: the run ends failed with the error, not queued.
func checkFinalAttemptFails(t *testing.T, h *taskHarness) {
	t.Helper()
	ctx := context.Background()
	h.taskFg.fail.Store(true)
	defer h.taskFg.fail.Store(false)
	var args jobs.TaskArgs
	err := h.st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		var ev store.TaskEvent
		if err := tx.QueryRow(ctx, `SELECT installation_id, repository_id FROM task_events LIMIT 1`).
			Scan(&ev.InstallationID, &ev.RepositoryID); err != nil {
			return err
		}
		ev.TenantID, ev.Forge, ev.Event, ev.RawEvent, ev.Action = h.tenant.ID(), "github", tasks.EventIssue, "issues", "opened"
		ev.Delivery, ev.SubjectKind, ev.SubjectNumber = "d-fail", "issue", 7
		id, err := store.InsertTaskEvent(ctx, tx, ev)
		if err != nil {
			return err
		}
		args = jobs.TaskArgs{TenantID: h.tenant.ID(), RepositoryID: ev.RepositoryID, EventID: id, Task: "triage", ConfigSHA: "c0ffee"}
		_, err = store.QueueTaskRun(ctx, tx, store.TaskRun{
			TenantID: h.tenant.ID(), RepositoryID: ev.RepositoryID, Task: "triage", EventID: id, SubjectKind: "issue", SubjectNumber: 7,
			Mode: "single", ConfigSHA: "c0ffee",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Insert(ctx, args, &river.InsertOpts{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	run := h.waitRuns(3)
	if run.status != "failed" || !strings.Contains(run.errText, "forge down") {
		t.Fatalf("a run out of attempts = %+v", run)
	}
}

// TestTaskRetriesAfterTimeout: a run whose model call its job's timeout cut
// short is retried, not failed, and answers on the retry.
func TestTaskRetriesAfterTimeout(t *testing.T) {
	h := newTaskHarness(t, time.Second)
	h.tm.block.Store(true)
	h.dispatch("d-timeout", "devin")
	waitFor(t, 20*time.Second, "the first attempt to time out", func() bool {
		var state string
		_ = h.st.App().QueryRow(context.Background(), `SELECT state FROM river_job WHERE kind = 'task' ORDER BY id DESC LIMIT 1`).
			Scan(&state)
		return state == "retryable"
	})
	if rs := h.runs(); len(rs) != 1 || store.TaskRunStatus(rs[0].status).Terminal() {
		t.Fatalf("a timed-out run = %+v, want it left to retry", rs)
	}
	h.tm.block.Store(false)
	if run := h.waitRuns(1); run.status != "succeeded" {
		t.Fatalf("the retried run = %+v", run)
	}
}
