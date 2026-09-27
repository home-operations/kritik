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
	"github.com/home-operations/kritik/internal/model"
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
      promptInline: "Triage issue #{{ .Subject.Number }}: {{ .Subject.Title }}\n{{ range .Context }}{{ . }}\n{{ end }}"
      agent: { maxSteps: 4, tools: [grep, run], commands: [cat] }
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

// agenticTaskHarness runs agentic tasks through the real ingest service,
// River, the local executor, the real runner and the gateway, over a
// taskForge on a real repository and a scripted model.
type agenticTaskHarness struct {
	*taskHarness
	ctx context.Context
	sm  *taskAgentModel
	// slug is the harness's tenant's, and the account and owner of its
	// repository, widgets.
	slug string
}

func newAgenticTaskHarness(t *testing.T, slug, extraContext string, embedder model.Embedder) *agenticTaskHarness {
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
	doc := strings.ReplaceAll(fmt.Sprintf(agenticTaskConfigYAML, srv.URL), "initech", slug)
	doc = strings.Replace(doc, `        commands: [{ name: other, run: "cat other.go" }]`,
		`        commands: [{ name: other, run: "cat other.go" }]`+extraContext, 1)
	file, err := configfile.Parse([]byte(doc))
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
	h.in, h.tenant, _ = file.Installation(slug + "-bot")
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
		Embedder: embedder, EmbedModel: "fake-embed",
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
	return &agenticTaskHarness{taskHarness: h, ctx: ctx, sm: sm, slug: slug}
}

// dispatch delivers the opening of issue #7.
func (h *agenticTaskHarness) dispatch(delivery string) {
	h.t.Helper()
	out, err := h.svc.Dispatch(h.ctx, ingest.Request{File: h.file, Tenant: h.tenant, Installation: h.in, Event: webhook.Event{
		Kind: webhook.KindIssue, Action: "opened", Delivery: delivery, Account: h.slug, Forge: configfile.ForgeGitHub,
		RawEvent: "issues", Sender: "devin", Raw: json.RawMessage(`{"action":"opened","issue":{"number":7}}`),
		Repository: &webhook.Repository{FullName: h.slug + "/widgets", DefaultBranch: "main"},
		Subject:    &webhook.Subject{Kind: webhook.SubjectIssue, Number: 7},
		Issue:      &webhook.Issue{Number: 7, Title: "It crashes", State: "open", Author: "devin"},
	}})
	if err != nil || out.Status != ingest.Enqueued {
		h.t.Fatalf("an issue an agentic task runs on = %+v, %v", out, err)
	}
}

func TestAgenticTaskEndToEnd(t *testing.T) {
	h := newAgenticTaskHarness(t, "initech", "", nil)
	ctx, st, sm, tf := h.ctx, h.st, h.sm, h.tf
	h.dispatch("agentic-d-1")
	var run taskRunRow
	var runnerRunID *string
	var taskRunID string
	var notes []string
	waitFor(t, 30*time.Second, "the agentic task run", func() bool {
		err := st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status, reason, model, error, coalesce(fields, 'null'), coalesce(applied, 'null'),
				coalesce(dropped, 'null'), comment_id, runner_run_id::text, id::text, notes FROM task_runs
				WHERE tenant_id = $1 AND task = 'agentic-triage'`, h.tenant.ID()).
				Scan(&run.status, &run.reason, &run.model, &run.errText, &run.fields, &run.applied, &run.dropped, &run.commentID, &runnerRunID,
					&taskRunID, &notes)
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
	// The runner's note on the glob it cut reached the task run.
	if !slices.Equal(notes, []string{"context files *.go kept 1 of 2 matches"}) {
		t.Fatalf("task run notes = %q", notes)
	}
	checkAgenticTaskRecords(t, h.taskHarness, taskRunID, *runnerRunID)
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
func checkAgenticTaskRecords(t *testing.T, h *taskHarness, taskRunID, runnerRunID string) {
	t.Helper()
	for _, c := range []struct {
		what, query string
		want        int
	}{
		{"submitted agent runs", `SELECT count(*) FROM agent_runs WHERE stop_reason = 'submitted' AND runner_run_id = '` + runnerRunID + `'`, 1},
		{"answered runs", `SELECT count(*) FROM task_runs WHERE id = '` + taskRunID + `' AND answered_at IS NOT NULL`, 1},
		{"done task runner runs", `SELECT count(*) FROM runner_runs WHERE kind = 'task' AND phase = 'done' AND id = '` + runnerRunID + `'`, 1},
		// usage has no task run column; this test's tenant runs only this task.
		{"task usage rows, one per step", `SELECT count(*) FROM usage WHERE tenant_id = '` + h.tenant.ID() +
			`' AND role = 'task' AND review_id IS NULL`, 2},
		{"agent step transcripts", `SELECT count(*) FROM model_calls WHERE kind = 'agent_step' AND task_run_id = '` + taskRunID + `'`, 2},
	} {
		if n := h.count(c.query); n != c.want {
			t.Fatalf("%s = %d, want %d", c.what, n, c.want)
		}
	}
	var tokens int
	if err := h.st.App().QueryRow(context.Background(), `SELECT count(*) FROM gateway_tokens WHERE runner_run_id = $1`, runnerRunID).
		Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("gateway tokens left = %d, %v", tokens, err)
	}
}

// TestSearchIndexHits checks a search source finds the chunks of the
// repository's completed index nearest its query, nearest first, k of
// them, and charges the query's embedding.
func TestSearchIndexHits(t *testing.T) {
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
	if err := st.EnsureIndexSchema(ctx, "kritik_app", "fake-embed", 8, false); err != nil {
		t.Fatalf("EnsureIndexSchema: %v", err)
	}
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "model-key")
	file, err := configfile.Parse([]byte(fmt.Sprintf(agenticTaskConfigYAML, "http://unused.invalid")))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatal(err)
	}
	in, tenant, _ := file.Installation("initech-bot")
	repoID := configfile.RepositoryID(in.ID(), "initech/widgets")
	fe := &fakeEmbedder{}
	chunks := seedIndex(t, st, tenant.ID(), repoID)
	r := &taskRunner{
		w:      &Task{Store: st, Logger: logger, Embedder: fe, EmbedModel: "fake-embed"},
		tenant: tenant, args: jobs.TaskArgs{RepositoryID: repoID}, settings: configfile.Settings{Limits: configfile.Limits{Concurrency: 1}},
		logger: logger,
	}
	hits, ok, err := r.searchIndex(ctx, tasks.NamedQuery{Name: "code", Query: "crashes on start", K: 2})
	if err != nil || !ok || len(hits) != 2 || hits[0].Path != "crash.go" || hits[0].Text != chunks[0].text {
		t.Fatalf("searchIndex = %+v, %v, %v", hits, ok, err)
	}
	// The source's budget keeps the nearest hit alone.
	b := &tasks.Budget{PerSource: len(mustMarshal(t, hits[0])) + 2, Left: 1 << 20}
	if kept := tasks.TakeList(b, "code", hits); len(kept) != 1 || kept[0].Path != "crash.go" || len(b.Notes) != 1 {
		t.Fatalf("TakeList = %+v, notes %q", kept, b.Notes)
	}
	var embedded int
	if err := st.WithTenant(ctx, tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM usage WHERE tenant_id = $1 AND role = 'embedding' AND review_id IS NULL`, tenant.ID()).
			Scan(&embedded)
	}); err != nil || embedded != 1 {
		t.Fatalf("embedding usage rows = %d, %v", embedded, err)
	}
}

// indexChunk is a chunk seedIndex writes.
type indexChunk struct{ path, text string }

// seedIndex makes a completed fake-embed index generation of three chunks
// the repository's active one, and returns the chunks.
func seedIndex(t *testing.T, st *store.Store, tenantID, repoID string) []indexChunk {
	t.Helper()
	ctx := context.Background()
	if err := st.EnsureIndexSchema(ctx, "kritik_app", "fake-embed", 8, false); err != nil {
		t.Fatalf("EnsureIndexSchema: %v", err)
	}
	chunks := []indexChunk{
		{"crash.go", "func crashOnStart() { panic(\"crashes on start\") }"},
		{"boot.go", "func boot() { start() }"},
		{"docs.md", "zzzz qqqq xxxx"},
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.text
	}
	// Its own embedder, so a test's counts only the searches.
	vectors, _, err := (&fakeEmbedder{}).Embed(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	err = st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var runID string
		if err := tx.QueryRow(ctx, `INSERT INTO index_runs (tenant_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
			VALUES ($1, $2, 'c0ffee', 'fake-embed', 8, 'full', 'completed') RETURNING id`, tenantID, repoID).Scan(&runID); err != nil {
			return err
		}
		for i, c := range chunks {
			if _, err := tx.Exec(ctx, `INSERT INTO index_chunks (tenant_id, repository_id, index_run_id, path, start_line, end_line, text, embedding)
				VALUES ($1, $2, $3, $4, 1, 1, $5, $6::halfvec)`, tenantID, repoID, runID, c.path, c.text, model.VectorLiteral(vectors[i])); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE repositories SET active_index_run_id = $2 WHERE id = $1`, repoID, runID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}

// TestAgenticTaskSnoozesBeforeBilling checks an agentic task whose model
// has no free slot snoozes before it spends anything, its index search's
// embedding included, and charges that embedding once when it runs.
func TestAgenticTaskSnoozesBeforeBilling(t *testing.T) {
	fe := &fakeEmbedder{}
	h := newAgenticTaskHarness(t, "hooli", `
        search: [{ name: code, query: "{{ .Subject.Title }}", k: 2 }]`, fe)
	ctx := h.ctx
	seedIndex(t, h.st, h.tenant.ID(), configfile.RepositoryID(h.in.ID(), "hooli/widgets"))
	// Another job holds the tenant's one slot on the task's model.
	held, err := takeLease(ctx, h.st, h.tenant.ID(), "gateway/agent-model", 1, -1)
	if err != nil || held == nil {
		t.Fatalf("takeLease = %v, %v", held, err)
	}
	h.dispatch("snooze-d-1")
	waitFor(t, 20*time.Second, "the task job to snooze", func() bool {
		var n int
		err := h.st.App().QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = $1 AND (metadata->>'snoozes')::int >= 1`,
			jobs.TaskArgs{}.Kind()).Scan(&n)
		return err == nil && n == 1
	})
	usage := func(role string) int {
		return h.count(`SELECT count(*) FROM usage WHERE tenant_id = '` + h.tenant.ID() + `' AND role LIKE '` + role + `'`)
	}
	fe.mu.Lock()
	calls := fe.calls
	fe.mu.Unlock()
	if n := usage("%"); n != 0 || calls != 0 {
		t.Fatalf("a snoozed task spent: %d usage rows, %d embedding calls", n, calls)
	}
	if err := held.release(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	waitFor(t, 30*time.Second, "the snoozed task to run", func() bool {
		err := h.st.WithTenant(ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM task_runs WHERE tenant_id = $1 AND task = 'agentic-triage'`, h.tenant.ID()).Scan(&status)
		})
		return err == nil && store.TaskRunStatus(status).Terminal()
	})
	if status != "succeeded" {
		t.Fatalf("status = %q", status)
	}
	if n := usage("embedding"); n != 1 {
		t.Fatalf("embedding usage rows = %d, want 1", n)
	}
	h.sm.mu.Lock()
	prompt := h.sm.users[0]
	h.sm.mu.Unlock()
	if !strings.Contains(prompt, `<untrusted source="context:code">`) || !strings.Contains(prompt, "crash.go") {
		t.Fatalf("the prompt lacks the index search: %s", prompt)
	}
	if _, err := h.st.SweepTaskEvents(ctx, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
