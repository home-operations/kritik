//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
	"github.com/home-operations/kritik/internal/webhook"
)

const agenticTaskConfigYAML = `
providers:
  gateway:
    type: openai
    baseUrl: %s/v1
    apiKey: { env: TEST_SECRET }
defaults:
  runner:
    activeDeadlineSeconds: 60
  models:
    review: gateway/agent-model
  limits:
    concurrency: 1
  allow:
    tasks: { enabled: true }
  tasks:
    - name: agentic-triage
      mode: agentic
      on: [{ issue: [opened] }]
      promptInline: "Triage issue #{{ .Subject.Number }}: {{ .Subject.Title }}\n{{ .Context.files }}"
      agent: { maxSteps: 4, tools: [grep], commands: [cat] }
      context:
        files: [{ path: main.go }, { glob: "*.go", max: 1 }]
        commands: [{ name: other, run: "cat other.go" }]
      fields:
        priority: { type: string, enum: [low, high] }
      actions:
        labels: { propose: { add: [bug, enhancement], remove: [needs-triage] } }
        comment: { mode: sticky }
tenants:
  - slug: initech
    installations:
      - name: initech-bot
        forge: github
        account: initech
        app:
          clientId: Iv1.x
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
    repositories:
      - name: initech/widgets
        agent:
          commandTimeout: 5s
`

// taskAgentModel is an OpenAI-compatible endpoint that greps, then submits
// a triage answer, and keeps the user prompts it was sent.
type taskAgentModel struct {
	mu    sync.Mutex
	step  int
	users []string
	tools [][]string
}

func (m *taskAgentModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	m.mu.Lock()
	m.step++
	step := m.step
	m.tools = append(m.tools, names)
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			m.users = append(m.users, fmt.Sprint(msg.Content))
			break
		}
	}
	m.mu.Unlock()
	name, args := "grep", `{"pattern":"func"}`
	if step > 1 {
		name, args = "submit_answer",
			`{"summary":"A crash on start.","fields":{"priority":"high"},"labels":{"add":["bug"],"remove":["needs-triage"]},"comment":""}`
	}
	quoted, _ := json.Marshal(args)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":"agent-model",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c%d","type":"function",`+
		`"function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"cost":0.01}}`, step, name, quoted)
}

// agenticTaskForge is a taskForge over a real repository the runner clones.
type agenticTaskForge struct {
	*taskForge
	lf *localForge
}

func (f *agenticTaskForge) CloneURL(owner, repo string) string           { return f.lf.CloneURL(owner, repo) }
func (f *agenticTaskForge) GitToken(ctx context.Context) (string, error) { return f.lf.GitToken(ctx) }

func (f *agenticTaskForge) BranchTip(ctx context.Context, owner, repo, branch string) (string, string, error) {
	return f.lf.BranchTip(ctx, owner, repo, branch)
}

func (f *agenticTaskForge) FileAt(ctx context.Context, owner, repo, ref, path string) ([]byte, error) {
	return f.lf.FileAt(ctx, owner, repo, ref, path)
}

func TestAgenticTaskEndToEnd(t *testing.T) {
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
	runnerStore, err := store.Open(ctx, store.Options{AppURL: env(t, "KRITIK_TEST_RUNNER_URL"), Logger: logger})
	if err != nil {
		t.Fatalf("Open runner: %v", err)
	}
	t.Cleanup(runnerStore.Close)
	sm := &taskAgentModel{}
	srv := httptest.NewServer(sm)
	t.Cleanup(srv.Close)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "model-key")
	file, err := configfile.Parse([]byte(fmt.Sprintf(agenticTaskConfigYAML, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatal(err)
	}
	dir, base, head := testRepo(t)
	tf := &taskForge{labels: []string{"needs-triage"}}
	f := &agenticTaskForge{taskForge: tf, lf: &localForge{dir: dir, base: base, tip: head}}
	insertOnly, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	h := &taskHarness{t: t, st: st, file: file, svc: ingest.NewService(st, insertOnly), tf: tf}
	h.in, h.tenant, _ = file.Installation("initech-bot")
	current := configfile.NewCurrent(file)
	gateway := httptest.NewServer(&Gateway{
		Store: st, Current: current, Logger: logger, Proxy: http.NotFoundHandler(), Steppers: &Completers{Build: BuildStepper},
	})
	t.Cleanup(gateway.Close)
	base0 := Base{Store: st, Current: current, Forges: &forges{f: f}, Logger: logger}
	workers := river.NewWorkers()
	river.AddWorker(workers, &TaskDispatch{Base: base0})
	river.AddWorker(workers, &Task{
		Base: base0, Completers: &completers{c: &taskModel{}}, Executor: &executor.Local{Store: runnerStore},
		GatewayURL: gateway.URL, GatewayTokenTTL: time.Hour, superviseEvery: 50 * time.Millisecond,
	})
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

	out, err := h.svc.Dispatch(ctx, ingest.Request{File: file, Tenant: h.tenant, Installation: h.in, Event: webhook.Event{
		Kind: webhook.KindIssue, Action: "opened", Delivery: "agentic-d-1", Account: "initech", Forge: configfile.ForgeGitHub,
		RawEvent: "issues", Sender: "devin", Raw: json.RawMessage(`{"action":"opened","issue":{"number":7}}`),
		Repository: &webhook.Repository{FullName: "initech/widgets", DefaultBranch: "main"},
		Subject:    &webhook.Subject{Kind: webhook.SubjectIssue, Number: 7},
		Issue:      &webhook.Issue{Number: 7, Title: "It crashes", State: "open", Author: "devin"},
	}})
	if err != nil || out.Status != ingest.Enqueued {
		t.Fatalf("an issue an agentic task runs on = %+v, %v", out, err)
	}
	var run taskRunRow
	var runnerRunID *string
	waitFor(t, 30*time.Second, "the agentic task run", func() bool {
		err := st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status, reason, model, error, coalesce(fields, 'null'), coalesce(applied, 'null'),
				coalesce(dropped, 'null'), comment_id, runner_run_id::text FROM task_runs WHERE task = 'agentic-triage'`).
				Scan(&run.status, &run.reason, &run.model, &run.errText, &run.fields, &run.applied, &run.dropped, &run.commentID, &runnerRunID)
		})
		return err == nil && store.TaskRunStatus(run.status).Terminal()
	})
	if run.status != "succeeded" || run.model != "agent-model" || string(run.fields) != `{"priority": "high"}` || runnerRunID == nil ||
		run.commentID == nil {
		t.Fatalf("run = %+v (fields %s, applied %s, dropped %s, runner run %v)", run, run.fields, run.applied, run.dropped, runnerRunID)
	}
	labels, comments, _ := tf.state()
	if !slices.Equal(labels, []string{"bug"}) || !strings.Contains(comments[*run.commentID], tasks.StickyMarker("agentic-triage")) ||
		!strings.Contains(comments[*run.commentID], "A crash on start.") {
		t.Fatalf("forge after the run: labels %q, comments %v", labels, comments)
	}
	checkAgenticTaskPrompt(t, sm)
	checkAgenticTaskRecords(t, h, *runnerRunID)
	// Retention sweeps across tenants; the event is not left for the next
	// test's count.
	if _, err := st.SweepTaskEvents(ctx, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

// checkAgenticTaskPrompt checks the runner's agent got the worker's
// prompt, the context only the runner gathers, fenced, and the task's
// tools with its answer schema's submit tool.
func checkAgenticTaskPrompt(t *testing.T, sm *taskAgentModel) {
	t.Helper()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if len(sm.users) < 2 {
		t.Fatalf("model steps = %d", len(sm.users))
	}
	user := sm.users[0]
	for _, want := range []string{
		"Triage issue #7: It crashes",
		`<untrusted source="context:files">`,
		`"path":"main.go"`,
		`<untrusted source="context:other">`,
		"func c()",
		"kept 1 of 2 matches",
		`<untrusted source="subject">`,
	} {
		if !strings.Contains(user, want) {
			t.Fatalf("the agent's prompt lacks %q:\n%s", want, user)
		}
	}
	if !slices.Equal(sm.tools[0], []string{"grep", "run", "submit_answer"}) {
		t.Fatalf("tools = %q", sm.tools[0])
	}
}

// checkAgenticTaskRecords checks what the run left in the database: the
// agent's row, the gateway's usage and transcript charged to the task
// run, and no live gateway token.
func checkAgenticTaskRecords(t *testing.T, h *taskHarness, runnerRunID string) {
	t.Helper()
	if n := h.count(`SELECT count(*) FROM agent_runs WHERE stop_reason = 'submitted' AND runner_run_id = '` + runnerRunID + `'`); n != 1 {
		t.Fatalf("agent runs = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM runner_runs WHERE kind = 'task' AND phase = 'done' AND id = '` + runnerRunID + `'`); n != 1 {
		t.Fatalf("done task runner runs = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM usage WHERE role = 'task' AND review_id IS NULL`); n != 2 {
		t.Fatalf("task usage rows = %d, want one per step", n)
	}
	if n := h.count(`SELECT count(*) FROM model_calls m JOIN task_runs r ON r.id = m.task_run_id
		WHERE m.kind = 'agent_step' AND r.task = 'agentic-triage'`); n != 2 {
		t.Fatalf("agent step transcripts = %d", n)
	}
	var tokens int
	if err := h.st.App().QueryRow(context.Background(), `SELECT count(*) FROM gateway_tokens WHERE runner_run_id = $1`, runnerRunID).
		Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("gateway tokens left = %d, %v", tokens, err)
	}
}
