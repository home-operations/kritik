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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/webhook"
)

const agenticConfigYAML = `
providers:
  gateway:
    type: openai
    baseUrl: %s/v1
    apiKey: { env: TEST_SECRET }
defaults:
  models:
    review: gateway/agent-model
  limits:
    concurrency: 1
tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: github
        account: acme
        app:
          clientId: Iv1.x
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
    repositories:
      - name: acme/widgets
        mode: agentic
        agent:
          maxSteps: 6
  - slug: globex
    installations:
      - name: globex-bot
        forge: github
        account: globex
        app:
          clientId: Iv1.y
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
`

// agentJobTimeout is the harness client's JobTimeout.
const agentJobTimeout = 2 * time.Second

// modelScript is how scriptedModel answers.
type modelScript int

const (
	// scriptSubmit answers grep, then read_file, then submit_review.
	scriptSubmit modelScript = iota
	// scriptProse only ever answers in prose, so the agent never submits.
	scriptProse
	// scriptReject refuses the key and echoes it back in the error.
	scriptReject
	// scriptStall answers grep, then calls stalled and never answers again.
	scriptStall
)

// scriptedModel is an OpenAI-compatible chat completions endpoint.
type scriptedModel struct {
	mu       sync.Mutex
	script   modelScript
	step     int
	auth     []string
	systems  []string
	requests int
	// stalled runs once when scriptStall starts holding a request.
	stalled func()
}

func (m *scriptedModel) reset(script modelScript) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.script, m.step = script, 0
}

func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m.mu.Lock()
	m.requests++
	m.step++
	step, script := m.step, m.script
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		m.systems = append(m.systems, fmt.Sprint(req.Messages[0].Content))
	}
	m.mu.Unlock()

	if script == scriptStall && step > 1 {
		m.mu.Lock()
		stalled := m.stalled
		m.stalled = nil
		m.mu.Unlock()
		if stalled != nil {
			stalled()
		}
		<-r.Context().Done()
		return
	}
	if script == scriptReject {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"invalid api key %s","type":"auth"}}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		return
	}
	message := `{"role":"assistant","content":"Still looking."}`
	finish := "stop"
	tool := func(name, args string) string {
		b, _ := json.Marshal(args)
		return fmt.Sprintf(`{"role":"assistant","content":null,"tool_calls":[{"id":"c%d","type":"function","function":{"name":%q,"arguments":%s}}]}`,
			step, name, b)
	}
	if script == scriptSubmit || script == scriptStall {
		finish = "tool_calls"
		switch step {
		case 1:
			message = tool("grep", `{"pattern":"func b"}`)
		case 2:
			message = tool("read_file", `{"path":"main.go"}`)
		default:
			message = tool("submit_review", `{"summary":{"take":"Adds b.","praise":[]},"findings":[`+
				`{"path":"main.go","line":3,"severity":"important","title":"b is unused","explanation":"Nothing calls b."}]}`)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":"agent-model",`+
		`"choices":[{"index":0,"message":%s,"finish_reason":%q}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"cost":0.01}}`, message, finish)
}

type agentRunRow struct {
	stop, model, errText string
	steps                int
	toolCalls, timeline  string
	input, output        int64
	cost                 float64
}

// agenticHarness drives agentic reviews of acme/widgets#1 through River,
// the local executor and the real runner.
type agenticHarness struct {
	ctx    context.Context
	st     *store.Store
	svc    *ingest.Service
	file   *configfile.File
	in     *configfile.Installation
	tenant *configfile.Tenant
	other  *configfile.Tenant
	lf     *localForge
	exec   *hookExecutor
	review *Review
	fc     *fakeCompleter
	sm     *scriptedModel
	dir    string
	base   string
	head   string
}

func newAgenticHarness(t *testing.T) *agenticHarness {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	appStore, err := store.Open(ctx, store.Options{
		AppURL: env(t, "KRITIK_TEST_APP_URL"), OwnerURL: env(t, "KRITIK_TEST_OWNER_URL"),
		AppRole: "kritik_app", RunnerRole: "kritik_runner", Logger: logger,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(appStore.Close)
	if err := appStore.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	runnerStore, err := store.Open(ctx, store.Options{AppURL: env(t, "KRITIK_TEST_RUNNER_URL"), Logger: logger})
	if err != nil {
		t.Fatalf("Open runner: %v", err)
	}
	t.Cleanup(runnerStore.Close)

	h := &agenticHarness{ctx: ctx, st: appStore, sm: &scriptedModel{}, fc: &fakeCompleter{}}
	srv := httptest.NewServer(h.sm)
	t.Cleanup(srv.Close)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "model-key")
	if h.file, err = configfile.Parse([]byte(fmt.Sprintf(agenticConfigYAML, srv.URL))); err != nil {
		t.Fatal(err)
	}
	if err := appStore.ApplyConfig(ctx, h.file, "test"); err != nil {
		t.Fatal(err)
	}
	h.in, h.tenant, _ = h.file.Installation("acme-bot")
	_, h.other, _ = h.file.Installation("globex-bot")
	h.dir, h.base, h.head = testRepo(t)
	h.lf = &localForge{dir: h.dir, base: h.base, tip: h.head}
	h.exec = &hookExecutor{inner: &executor.Local{Store: runnerStore}}

	insertOnly, err := river.NewClient(riverpgxv5.New(appStore.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = ingest.NewService(appStore, insertOnly)
	workers := river.NewWorkers()
	h.review = &Review{
		Store: appStore, Current: configfile.NewCurrent(h.file), Forges: &forges{f: h.lf}, Completers: &completers{c: h.fc},
		Executor: h.exec, Deadline: time.Minute, Logger: logger, superviseEvery: 50 * time.Millisecond,
	}
	river.AddWorker(workers, h.review)
	client, err := river.NewClient(riverpgxv5.New(appStore.App()), &river.Config{
		Queues: map[string]river.QueueConfig{jobs.QueueReview: {MaxWorkers: 1}}, Workers: workers,
		FetchCooldown: 50 * time.Millisecond, FetchPollInterval: 100 * time.Millisecond,
		// Far shorter than any review: the worker's own Timeout must win.
		JobTimeout: agentJobTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background()) })
	return h
}

func (h *agenticHarness) dispatch(t *testing.T, headSHA string) {
	h.dispatchBody(t, headSHA, "Adds b.")
}

func (h *agenticHarness) dispatchBody(t *testing.T, headSHA, body string) {
	t.Helper()
	out, err := h.svc.Dispatch(h.ctx, ingest.Request{File: h.file, Tenant: h.tenant, Installation: h.in, Event: webhook.Event{
		Kind: webhook.KindPullRequest, Action: "synchronize", Account: "acme",
		Repository: &webhook.Repository{FullName: "acme/widgets", DefaultBranch: "main"},
		PullRequest: &webhook.PullRequest{Number: 1, Title: "Add b", Body: body, Author: "octocat", State: "open",
			HeadRef: "f", HeadSHA: headSHA, BaseRef: "main"},
	}})
	if err != nil || out.Status != ingest.Enqueued {
		t.Fatalf("dispatch = %+v, %v", out, err)
	}
}

func (h *agenticHarness) waitReview(t *testing.T, headSHA string) (id, status, errText string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(h.ctx, `SELECT id, status, error FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL
				ORDER BY created_at DESC LIMIT 1`, headSHA).Scan(&id, &status, &errText)
		})
		if err == nil {
			return id, status, errText
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no finished review for %s", headSHA)
	return "", "", ""
}

func (h *agenticHarness) agentRow(t *testing.T, reviewID string) agentRunRow {
	t.Helper()
	var r agentRunRow
	err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT a.stop_reason, a.model, a.error, a.steps, a.tool_calls::text, a.timeline::text,
			a.input_tokens, a.output_tokens, a.cost_usd::float8
			FROM agent_runs a JOIN runner_runs r ON r.id = a.runner_run_id WHERE r.review_id = $1`, reviewID).
			Scan(&r.stop, &r.model, &r.errText, &r.steps, &r.toolCalls, &r.timeline, &r.input, &r.output, &r.cost)
	})
	if err != nil {
		t.Fatalf("agent run for review %s: %v", reviewID, err)
	}
	return r
}

func TestAgenticReviewEndToEnd(t *testing.T) {
	h := newAgenticHarness(t)
	t.Run("the agent greps, reads and submits a finding", func(t *testing.T) { checkAgentSubmits(t, h) })
	t.Run("an agent that never submits fails the review and says so", func(t *testing.T) { checkAgentNeverSubmits(t, h) })
	t.Run("a run superseded after the Job still charges its tokens", func(t *testing.T) { checkAgentSupersededCharges(t, h) })
	t.Run("a key the provider echoes back is masked", func(t *testing.T) { checkAgentKeyMasked(t, h) })
	t.Run("the merge-base filter skips before the agent runs", func(t *testing.T) { checkAgentFiltered(t, h) })
	t.Run("a runner skip the worker does not repeat still sets the status", func(t *testing.T) { checkRunnerOnlySkip(t, h) })
	t.Run("a review outlives the client's job timeout", func(t *testing.T) { checkAgentOutlivesJobTimeout(t, h) })
	t.Run("an agent cancelled mid-run still charges its tokens", func(t *testing.T) { checkAgentCanceledCharges(t, h) })
	t.Run("a run that never got a Job is failed, not left created", func(t *testing.T) { checkFailRun(t, h) })
	t.Run("a capped review is capped under the lease and lets it go", func(t *testing.T) { checkAgentCappedUnderLease(t, h) })
	t.Run("another tenant cannot read the agent runs", func(t *testing.T) {
		count := func(tenantID string) int {
			var n int
			if err := h.st.WithTenant(h.ctx, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(h.ctx, `SELECT count(*) FROM agent_runs`).Scan(&n)
			}); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if own, foreign := count(h.tenant.ID()), count(h.other.ID()); own != 8 || foreign != 0 {
			t.Fatalf("acme sees %d agent runs, globex sees %d", own, foreign)
		}
	})
}

func checkAgentSubmits(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	h.dispatch(t, h.head)
	reviewID, status, errText := h.waitReview(t, h.head)
	if status != "completed" {
		t.Fatalf("status = %s (%s)", status, errText)
	}
	var mode string
	if err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT mode FROM reviews WHERE id = $1`, reviewID).Scan(&mode)
	}); err != nil || mode != "agentic" {
		t.Fatalf("review mode = %q, %v", mode, err)
	}
	run := h.agentRow(t, reviewID)
	var tools map[string]int
	var timeline []map[string]any
	_ = json.Unmarshal([]byte(run.toolCalls), &tools)
	_ = json.Unmarshal([]byte(run.timeline), &timeline)
	if run.stop != "submitted" || run.steps != 3 || run.model != "agent-model" || tools["grep"] != 1 || tools["read_file"] != 1 ||
		tools["submit_review"] != 1 || len(timeline) != 3 || run.input != 300 || run.output != 30 {
		t.Fatalf("agent run = %+v", run)
	}
	var findings, usage int
	var usageModel string
	var tokens int64
	err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(h.ctx, `SELECT count(*) FROM findings WHERE review_id = $1 AND path = 'main.go' AND line = 3
			AND posted_inline`, reviewID).Scan(&findings); err != nil {
			return err
		}
		return tx.QueryRow(h.ctx, `SELECT count(*), max(model), sum(input_tokens + output_tokens) FROM usage
			WHERE review_id = $1 AND role = 'review'`, reviewID).Scan(&usage, &usageModel, &tokens)
	})
	if err != nil || findings != 1 || usage != 1 || usageModel != "agent-model" || tokens != 330 {
		t.Fatalf("findings=%d usage=%d model=%s tokens=%d err=%v", findings, usage, usageModel, tokens, err)
	}
	h.lf.mu.Lock()
	inline, comments, forgeStatus := inlineBodies(h.lf), len(h.lf.comments), h.lf.status
	sticky := h.lf.comments[commentBase+1]
	h.lf.mu.Unlock()
	if len(inline) != 1 || !strings.Contains(inline[0], "b is unused") || comments != 1 ||
		!strings.Contains(sticky, "/main.go#L3) b is unused") || forgeStatus != "success: kritik: 1 finding(s)" {
		t.Fatalf("inline=%v comments=%d status=%q sticky:\n%s", inline, comments, forgeStatus, sticky)
	}
	h.sm.mu.Lock()
	auth, system := h.sm.auth[0], h.sm.systems[0]
	h.sm.mu.Unlock()
	if auth != "Bearer model-key" || !strings.HasPrefix(system, "You are kritik") {
		t.Fatalf("auth=%q system=%.40q", auth, system)
	}
	h.fc.mu.Lock()
	calls := h.fc.calls
	h.fc.mu.Unlock()
	if calls != 0 {
		t.Fatalf("the worker's own model was called %d time(s) in agentic mode", calls)
	}
}

func checkAgentNeverSubmits(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptProse)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc c() {}\n")
	h.dispatch(t, next)
	reviewID, status, errText := h.waitReview(t, next)
	if status != "failed" || errText != "agent stopped: no_submit" {
		t.Fatalf("status = %s, error = %q", status, errText)
	}
	if run := h.agentRow(t, reviewID); run.stop != "no_submit" || run.steps != 2 {
		t.Fatalf("agent run = %+v", run)
	}
	var usage int
	err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM usage WHERE review_id = $1 AND role = 'review' AND input_tokens = 200`, reviewID).
			Scan(&usage)
	})
	if err != nil || usage != 1 {
		t.Fatalf("an unsubmitted run still spent tokens: usage rows=%d err=%v", usage, err)
	}
	h.lf.mu.Lock()
	comments, sticky, forgeStatus := len(h.lf.comments), h.lf.comments[commentBase+1], h.lf.status
	h.lf.mu.Unlock()
	if comments != 1 || !strings.Contains(sticky, "**Review incomplete for `"+next[:7]+"`:** agent stopped: no_submit.") ||
		strings.Contains(sticky, "b is unused") || !strings.Contains(forgeStatus, "incomplete") {
		t.Fatalf("comments=%d status=%q sticky:\n%s", comments, forgeStatus, sticky)
	}
}

// inlineBodies lists the inline comment bodies; the caller holds the lock.
func inlineBodies(l *localForge) []string {
	out := make([]string, len(l.inline))
	for i, c := range l.inline {
		out[i] = c.Body
	}
	return out
}

// hookExecutor runs the real runner and then, once, a hook: what happens
// right after a Job ends and before the worker looks at the pull request.
type hookExecutor struct {
	inner executor.Executor

	mu    sync.Mutex
	after func()
	// hold delays the next run before it starts, as a slow node would.
	hold time.Duration
	// detach makes the next run end the way a deleted pod does: Run
	// returns as soon as ctx ends, and the runner only sees the
	// cancellation a moment later, as a terminating pod would.
	detach bool
}

func (e *hookExecutor) Run(ctx context.Context, spec executor.Spec) executor.Result {
	e.mu.Lock()
	hold, detach := e.hold, e.detach
	e.hold, e.detach = 0, false
	e.mu.Unlock()
	if detach {
		return e.runDetached(ctx, spec)
	}
	select {
	case <-time.After(hold):
	case <-ctx.Done():
		return executor.Result{JobName: "kritik-run-held", Err: context.Cause(ctx)}
	}
	res := e.inner.Run(ctx, spec)
	e.mu.Lock()
	after := e.after
	e.after = nil
	e.mu.Unlock()
	if after != nil {
		after()
	}
	return res
}

func (e *hookExecutor) runDetached(ctx context.Context, spec executor.Spec) executor.Result {
	ictx, icancel := context.WithCancelCause(context.WithoutCancel(ctx))
	done := make(chan executor.Result, 1)
	go func() { done <- e.inner.Run(ictx, spec) }()
	select {
	case res := <-done:
		icancel(nil)
		return res
	case <-ctx.Done():
		cause := context.Cause(ctx)
		time.AfterFunc(300*time.Millisecond, func() { icancel(cause) })
		return executor.Result{JobName: "kritik-run-deleted", Err: cause}
	}
}

// commit writes one file on top of the test repository's HEAD.
func (h *agenticHarness) commit(t *testing.T, name, content string) string {
	t.Helper()
	r, err := git.PlainOpen(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = wt.Add(name)
	c, err := wt.Commit("change "+name, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@x", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// usageTokens is the review's review-role usage: rows and tokens.
func (h *agenticHarness) usageTokens(t *testing.T, reviewID string) (rows int, tokens int64) {
	t.Helper()
	err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*), coalesce(sum(input_tokens + output_tokens), 0) FROM usage
			WHERE review_id = $1 AND role = 'review'`, reviewID).Scan(&rows, &tokens)
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows, tokens
}

func checkAgentSupersededCharges(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc d() {}\n")
	h.exec.mu.Lock()
	h.exec.after = func() {
		// A push lands as the Job ends.
		err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(h.ctx, `UPDATE pull_requests SET head_sha = $1 WHERE number = 1`, strings.Repeat("f", 40))
			return err
		})
		if err != nil {
			t.Error(err)
		}
	}
	h.exec.mu.Unlock()
	h.dispatch(t, next)
	reviewID, status, _ := h.waitReview(t, next)
	if status != "superseded" {
		t.Fatalf("status = %s, want superseded", status)
	}
	if run := h.agentRow(t, reviewID); run.stop != "submitted" {
		t.Fatalf("agent run = %+v", run)
	}
	if rows, tokens := h.usageTokens(t, reviewID); rows != 1 || tokens != 330 {
		t.Fatalf("a superseded agent run must still be charged: rows=%d tokens=%d", rows, tokens)
	}
}

func checkAgentKeyMasked(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptReject)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc e() {}\n")
	h.dispatch(t, next)
	reviewID, status, errText := h.waitReview(t, next)
	run := h.agentRow(t, reviewID)
	if status != "failed" || run.stop != "error" || !strings.Contains(run.errText, "invalid api key ***") ||
		strings.Contains(run.errText, "model-key") || strings.Contains(errText, "model-key") || !strings.Contains(errText, "***") {
		t.Fatalf("status=%s review error=%q agent error=%q", status, errText, run.errText)
	}
	if rows, _ := h.usageTokens(t, reviewID); rows != 1 {
		t.Fatalf("usage rows = %d", rows)
	}
}

func checkAgentFiltered(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	h.sm.mu.Lock()
	before := h.sm.requests
	h.sm.mu.Unlock()
	base := h.commit(t, ".kritik.yaml", "filter: '!pr.body.contains(\"[skip-review]\")'\n")
	h.lf.setBase(base)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc f() {}\n")
	h.dispatchBody(t, next, "Adds f. [skip-review]")
	reviewID, status, _ := h.waitReview(t, next)
	if status != "skipped" {
		t.Fatalf("status = %s, want skipped", status)
	}
	if run := h.agentRow(t, reviewID); run.stop != "skipped" || run.errText != "filtered" || run.steps != 0 {
		t.Fatalf("agent run = %+v", run)
	}
	h.sm.mu.Lock()
	after := h.sm.requests
	h.sm.mu.Unlock()
	if rows, _ := h.usageTokens(t, reviewID); after != before || rows != 0 {
		t.Fatalf("a filtered review called the model %d time(s) and has %d usage row(s)", after-before, rows)
	}
}

func checkAgentOutlivesJobTimeout(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc g() {}\n")
	h.exec.mu.Lock()
	h.exec.hold = 2 * agentJobTimeout
	h.exec.mu.Unlock()
	started := time.Now()
	h.dispatch(t, next)
	_, status, errText := h.waitReview(t, next)
	if status != "completed" || time.Since(started) < 2*agentJobTimeout {
		t.Fatalf("status = %s (%s) after %s", status, errText, time.Since(started))
	}
	var reviews int
	err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM reviews WHERE head_sha = $1`, next).Scan(&reviews)
	})
	if err != nil || reviews != 1 {
		t.Fatalf("the review was cut off and retried: %d review rows, err=%v", reviews, err)
	}
}

// checkAgentCappedUnderLease caps a review on the tenant's daily count,
// which earlier subtests have already spent, and checks the lease taken to
// read the caps is released rather than held for the capped review.
func checkAgentCappedUnderLease(t *testing.T, h *agenticHarness) {
	capped := *h.file
	capped.Defaults.Limits.ReviewsPerDay = 1
	h.review.Current.Set(&capped)
	t.Cleanup(func() { h.review.Current.Set(h.file) })
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc capped() {}\n")
	h.dispatch(t, next)
	_, status, errText := h.waitReview(t, next)
	if status != statusCapped || !strings.Contains(errText, "reviewsPerDay") {
		t.Fatalf("status = %s (%s), want capped on reviewsPerDay", status, errText)
	}
	var held int
	err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM model_leases WHERE job_id IS NOT NULL`).Scan(&held)
	})
	if err != nil || held != 0 {
		t.Fatalf("%d leases still held after a capped review, err=%v", held, err)
	}
}

func checkAgentCanceledCharges(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptStall)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc h() {}\n")
	h.sm.mu.Lock()
	h.sm.stalled = func() {
		// A push lands while the agent waits on its second step, and
		// supervision cancels the run.
		err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(h.ctx, `UPDATE pull_requests SET head_sha = $1 WHERE number = 1`, strings.Repeat("e", 40))
			return err
		})
		if err != nil {
			t.Error(err)
		}
	}
	h.sm.mu.Unlock()
	h.exec.mu.Lock()
	h.exec.detach = true
	h.exec.mu.Unlock()
	h.dispatch(t, next)
	reviewID, status, _ := h.waitReview(t, next)
	if status != "superseded" {
		t.Fatalf("status = %s, want superseded", status)
	}
	if run := h.agentRow(t, reviewID); run.stop != "canceled" || run.steps != 1 || run.input != 100 || run.output != 10 {
		t.Fatalf("agent run = %+v", run)
	}
	if rows, tokens := h.usageTokens(t, reviewID); rows != 1 || tokens != 110 {
		t.Fatalf("a cancelled agent run must still be charged: rows=%d tokens=%d", rows, tokens)
	}
}

func checkFailRun(t *testing.T, h *agenticHarness) {
	args := jobs.ReviewArgs{TenantID: h.tenant.ID(), RepositoryID: configfile.RepositoryID(h.in.ID(), "acme/widgets"), Number: 1,
		HeadSHA: strings.Repeat("d", 40), Trigger: "test"}
	pr, err := h.review.load(h.ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	_, runID, _, err := h.review.start(h.ctx, args, pr, h.base, configfile.ReviewAgentic)
	if err != nil {
		t.Fatal(err)
	}
	if err := failRun(h.ctx, h.st, h.tenant.ID(), runID, "worker: read pull request for the filter: boom"); err != nil {
		t.Fatal(err)
	}
	var phase, errText string
	var finished bool
	err = h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT phase, error, finished_at IS NOT NULL FROM runner_runs WHERE id = $1`, runID).
			Scan(&phase, &errText, &finished)
	})
	if err != nil || phase != "failed" || !strings.Contains(errText, "boom") || !finished {
		t.Fatalf("run phase=%q error=%q finished=%v err=%v", phase, errText, finished, err)
	}
}

func checkRunnerOnlySkip(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc k() {}\n")
	h.lf.mu.Lock()
	h.lf.status = ""
	h.lf.mu.Unlock()
	h.exec.mu.Lock()
	h.exec.after = func() {
		// The body is edited as the Job ends: the runner saw the skip
		// marker, the worker's own filter check does not.
		err := h.st.WithTenant(h.ctx, h.tenant.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(h.ctx, `UPDATE pull_requests SET body = 'Adds k.' WHERE number = 1`)
			return err
		})
		if err != nil {
			t.Error(err)
		}
	}
	h.exec.mu.Unlock()
	h.dispatchBody(t, next, "Adds k. [skip-review]")
	reviewID, status, _ := h.waitReview(t, next)
	if status != "skipped" {
		t.Fatalf("status = %s, want skipped", status)
	}
	if run := h.agentRow(t, reviewID); run.stop != "skipped" || run.errText != "filtered" {
		t.Fatalf("agent run = %+v", run)
	}
	h.lf.mu.Lock()
	forgeStatus := h.lf.status
	h.lf.mu.Unlock()
	if forgeStatus != "success: kritik: skipped (filtered by .kritik.yaml)" {
		t.Fatalf("forge status = %q", forgeStatus)
	}
}
