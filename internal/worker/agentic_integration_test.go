//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritika/internal/adapter"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
	"github.com/home-operations/kritika/internal/contextpack"
	"github.com/home-operations/kritika/internal/executor"
	"github.com/home-operations/kritika/internal/gateway"
	"github.com/home-operations/kritika/internal/gitfetch"
	"github.com/home-operations/kritika/internal/ingest"
	"github.com/home-operations/kritika/internal/jobs"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/runner"
	"github.com/home-operations/kritika/internal/store"
	"github.com/home-operations/kritika/internal/store/storetest"
	"github.com/home-operations/kritika/internal/transcript"
	"github.com/home-operations/kritika/internal/webhook"
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
apps:
  acme-bot:
    accounts: [acme]
    clientId: Iv1.x
    privateKey: { env: TEST_PEM }
    webhookSecret: { env: TEST_SECRET }
  globex-bot:
    accounts: [globex]
    clientId: Iv1.y
    privateKey: { env: TEST_PEM }
    webhookSecret: { env: TEST_SECRET }
repositories:
  acme/widgets:
    agent:
      maxSteps: 6
      commands: [curl]
      commandTimeout: 5s
    rules:
      - { id: into-main, rule: Keep main releasable., whenExpr: 'pr.baseRef == "main"' }
      - { id: renovate, rule: Say what the update breaks., whenExpr: 'pr.headRef.startsWith("renovate/")' }
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
	// scriptRun has curl fetch a release page, then submits.
	scriptRun
)

// scriptedModel is an OpenAI-compatible chat completions endpoint.
type scriptedModel struct {
	mu       sync.Mutex
	script   modelScript
	step     int
	auth     []string
	systems  []string
	requests int
	// toolResults are the contents of the tool messages the model was sent.
	toolResults []string
	// maxTokens is each request's answer cap.
	maxTokens []int64
	// stalled runs once when scriptStall starts holding a request.
	stalled func()
	// bodies are the requests as the provider received them.
	bodies [][]byte
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
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	m.mu.Lock()
	m.bodies = append(m.bodies, body)
	m.requests++
	m.step++
	step, script := m.step, m.script
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	m.maxTokens = append(m.maxTokens, req.MaxCompletionTokens)
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		m.systems = append(m.systems, fmt.Sprint(req.Messages[0].Content))
	}
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			m.toolResults = append(m.toolResults, fmt.Sprint(msg.Content))
		}
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
	if script == scriptRun {
		finish = "tool_calls"
		message = tool("run", `{"command":"curl","args":["-sS","https://releases.example.com/b/v2"]}`)
		if step > 1 {
			message = tool("submit_review", `{"summary":{"take":"Bumps b to v2.","praise":[]},"findings":[]}`)
		}
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
				`{"path":"main.go","line":3,"severity":"important","category":"correctness","title":"b is unused","explanation":"Nothing calls b."}]}`)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"x","object":"chat.completion","created":1,"model":"agent-model",`+
		`"choices":[{"index":0,"message":%s,"finish_reason":%q}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"cost":0.01}}`, message, finish)
}

type agentRunRow struct {
	stop, model, errText         string
	steps                        int
	toolCalls, timeline, sources string
	input, output                int64
	cost                         float64
}

// agenticHarness drives agentic reviews of acme/widgets#1 through River,
// the local executor and the real runner.
type agenticHarness struct {
	ctx context.Context
	st  *store.Store
	svc *ingest.Service
	// insertOnly enqueues jobs without working them, as the web API does.
	insertOnly *river.Client[pgx.Tx]
	file       *configfile.File
	account    *configfile.Account
	other      *configfile.Account
	lf         *localForge
	exec       *hookExecutor
	review     *Review
	// gatewayURL is the worker's gateway the runner calls its model
	// through, which calls sm with the account's key.
	gatewayURL string
	sm         *scriptedModel
	// fe is the gateway's embedder, used once the configuration names one.
	fe *fakeEmbedder
	// config is the configuration file h.file was parsed from.
	config string
	dir    string
	base   string
	head   string
}

func newAgenticHarness(t *testing.T) *agenticHarness {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	appStore := storetest.Open(t)
	runnerStore, err := store.Open(ctx, store.Options{AppURL: storetest.Env(t, "KRITIKA_TEST_RUNNER_URL"), Logger: logger})
	if err != nil {
		t.Fatalf("Open runner: %v", err)
	}
	t.Cleanup(runnerStore.Close)

	h := &agenticHarness{ctx: ctx, st: appStore, sm: &scriptedModel{}, fe: &fakeEmbedder{}}
	srv := httptest.NewServer(h.sm)
	t.Cleanup(srv.Close)
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "model-key")
	t.Setenv("KRITIKA_RUNNER_DEADLINE", "60s")
	t.Setenv("KRITIKA_RUNNER_TOOLS", `[{"name": "helm", "image": "registry.example/helm:3"},
		{"name": "kurl", "image": "registry.example/kurl:1", "path": "/usr/bin", "commands": ["curl"]}]`)
	// Credentials in the provider's URL, which the SDK prints in its errors,
	// must not reach a runner either.
	providerURL := strings.Replace(srv.URL, "http://", "http://kritika:provider-secret@", 1)
	h.config = fmt.Sprintf(agenticConfigYAML, providerURL)
	if h.file, err = configfiletest.Parse(t, h.config); err != nil {
		t.Fatal(err)
	}
	if err := appStore.ApplyConfig(ctx, h.file); err != nil {
		t.Fatal(err)
	}
	h.account, _ = h.file.Account(configfile.ForgeGitHub, "acme")
	h.other, _ = h.file.Account(configfile.ForgeGitHub, "globex")
	h.dir, h.base, h.head = testRepo(t)
	// The run tool's curl: prints what it was given and fetches nothing.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\necho fetched \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.lf = &localForge{dir: h.dir, base: h.base, tip: h.head}
	h.exec = &hookExecutor{inner: &executor.Local{Store: runnerStore}}

	insertOnly, err := river.NewClient(riverpgxv5.New(appStore.App()), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = ingest.NewService(appStore, insertOnly)
	h.insertOnly = insertOnly
	workers := river.NewWorkers()
	h.review = &Review{
		Store: appStore, Current: configfile.NewCurrent(h.file), Forges: &forges{f: h.lf},
		Executor: h.exec, Logger: logger, superviseEvery: 50 * time.Millisecond,
	}
	gw := httptest.NewServer(&gateway.Server{
		Store: appStore, Current: h.review.Current, Logger: logger,
		Proxy: http.NotFoundHandler(), Steppers: &adapter.Steppers{Build: adapter.BuildStepper},
		Embedders: &adapter.Embedders{Build: func(configfile.Embedding) model.Embedder { return h.fe }},
	})
	t.Cleanup(gw.Close)
	h.gatewayURL = gw.URL
	h.review.GatewayURL, h.review.GatewayTokenTTL = gw.URL, time.Hour
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
	h.dispatchAs(t, headSHA, body, false)
}

// dispatchAs pushes headSHA to the pull request, authored by a bot when
// bot is set.
func (h *agenticHarness) dispatchAs(t *testing.T, headSHA, body string, bot bool) {
	t.Helper()
	out, err := h.svc.Dispatch(h.ctx, ingest.Request{File: h.file, Account: h.account, Event: webhook.Event{
		Kind: webhook.KindPullRequest, Action: "synchronize", Account: "acme",
		Repository: &webhook.Repository{FullName: "acme/widgets", DefaultBranch: "main"},
		PullRequest: &webhook.PullRequest{Number: 1, Title: "Add b", Body: body, Author: "octocat", AuthorIsBot: bot, State: "open",
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
		err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
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
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT a.stop_reason, a.model, a.error, a.steps, a.tool_calls::text, a.timeline::text,
			a.sources::text, a.input_tokens, a.output_tokens, a.cost_usd::float8
			FROM agent_runs a JOIN runner_runs r ON r.id = a.runner_run_id WHERE r.review_id = $1`, reviewID).
			Scan(&r.stop, &r.model, &r.errText, &r.steps, &r.toolCalls, &r.timeline, &r.sources, &r.input, &r.output, &r.cost)
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
	t.Run("the merge-base filter skips before the runner starts", func(t *testing.T) { checkAgentFiltered(t, h) })
	t.Run("a runner skip the worker does not repeat still sets the status", func(t *testing.T) { checkRunnerOnlySkip(t, h) })
	t.Run("a diff over maxChangedLines is skipped unless asked for", func(t *testing.T) { checkTooLarge(t, h) })
	t.Run("a review outlives the client's job timeout", func(t *testing.T) { checkAgentOutlivesJobTimeout(t, h) })
	t.Run("an agent cancelled mid-run still charges its tokens", func(t *testing.T) { checkAgentCanceledCharges(t, h) })
	t.Run("a run that never got a Job is failed, not left created", func(t *testing.T) { checkFailRun(t, h) })
	t.Run("an agent spec cut short by the job ending is not retried", func(t *testing.T) { checkAgentSpecFailed(t, h) })
	t.Run("a capped review is capped under the lease and lets it go", func(t *testing.T) { checkAgentCappedUnderLease(t, h) })
	t.Run("the gateway serves a run token's steps within its budget", func(t *testing.T) { checkGatewayEndpoint(t, h) })
	t.Run("the gateway serves similar code into the agent's prompt", func(t *testing.T) { checkGatewaySimilar(t, h) })
	t.Run("an agentic review snoozes while every model slot is held", func(t *testing.T) { checkAgentSnoozes(t, h) })
	t.Run("the agent runs curl and the comment lists what it fetched", func(t *testing.T) { checkAgentRunsCommands(t, h) })
	t.Run("another account cannot read the agent runs", func(t *testing.T) {
		count := func(accountID string) int {
			var n int
			if err := h.st.WithAccount(h.ctx, accountID, func(tx pgx.Tx) error {
				return tx.QueryRow(h.ctx, `SELECT count(*) FROM agent_runs`).Scan(&n)
			}); err != nil {
				t.Fatal(err)
			}
			return n
		}
		// One review per check that ran an agent; the runner-only skips ran
		// none, and the too-large check's asked-for review ran one.
		if own, foreign := count(h.account.ID()), count(h.other.ID()); own != 10 || foreign != 0 {
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
	run := h.agentRow(t, reviewID)
	var tools map[string]int
	var timeline []map[string]any
	_ = json.Unmarshal([]byte(run.toolCalls), &tools)
	_ = json.Unmarshal([]byte(run.timeline), &timeline)
	h.exec.mu.Lock()
	mounted := h.exec.tools
	h.exec.mu.Unlock()
	if !slices.Equal(mounted, []string{"kurl"}) {
		t.Fatalf("tools handed to the executor = %v, want only kurl, which provides the curl acme/widgets allows", mounted)
	}
	if run.stop != "submitted" || run.steps != 3 || run.model != "agent-model" || tools["grep"] != 1 || tools["read_file"] != 1 ||
		tools["submit_review"] != 1 || len(timeline) != 3 || run.input != 300 || run.output != 30 {
		t.Fatalf("agent run = %+v", run)
	}
	var findings, usage int
	var usageModel string
	var tokens int64
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(h.ctx, `SELECT count(*) FROM findings WHERE review_id = $1 AND path = 'main.go' AND line = 3
			AND posted_inline`, reviewID).Scan(&findings); err != nil {
			return err
		}
		return tx.QueryRow(h.ctx, `SELECT count(*), max(model), sum(input_tokens + output_tokens) FROM usage
			WHERE review_id = $1 AND role = 'review'`, reviewID).Scan(&usage, &usageModel, &tokens)
	})
	// The gateway records each of the three steps as it serves it.
	if err != nil || findings != 1 || usage != 3 || usageModel != "agent-model" || tokens != 330 {
		t.Fatalf("findings=%d usage=%d model=%s tokens=%d err=%v", findings, usage, usageModel, tokens, err)
	}
	h.lf.mu.Lock()
	inline, comments, forgeStatus := inlineBodies(h.lf), len(h.lf.comments), h.lf.status
	sticky := h.lf.comments[commentBase+1]
	h.lf.mu.Unlock()
	if len(inline) != 1 || !strings.Contains(inline[0], "b is unused") || comments != 1 ||
		!strings.Contains(sticky, "/main.go#L3) [b is unused](local://acme/widgets/pull/1#r1001)") || forgeStatus != "success: kritika: 1 finding(s)" {
		t.Fatalf("inline=%v comments=%d status=%q sticky:\n%s", inline, comments, forgeStatus, sticky)
	}
	h.sm.mu.Lock()
	auth, system := h.sm.auth[0], h.sm.systems[0]
	h.sm.mu.Unlock()
	if auth != "Bearer model-key" || !strings.HasPrefix(system, "You are kritika") {
		t.Fatalf("auth=%q system=%.40q", auth, system)
	}
	if !strings.Contains(system, "\n\n## Repository instructions\n\n") || !strings.HasSuffix(system, "\n\nKeep functions small.") {
		t.Fatalf("the runner left the root's AGENTS.md out of the system prompt:\n%s", system)
	}
	checkAgentRules(t, system)
	checkAgentTranscript(t, h, reviewID)
}

// checkAgentTranscript checks that the gateway recorded a review's steps
// so that they rebuild into the conversation the provider was last sent,
// with no secret in any column and nothing another account can read.
func checkAgentTranscript(t *testing.T, h *agenticHarness, reviewID string) {
	t.Helper()
	rows := h.modelCalls(t, h.account.ID(), reviewID)
	if len(rows) != 3 {
		t.Fatalf("%d model calls recorded, want 3", len(rows))
	}
	for i, r := range rows {
		if r.Kind != store.ModelCallAgentStep || r.Step != i || r.Model != "agent-model" || r.Usage.Input != 100 || r.CostUSD != 0.01 {
			t.Fatalf("model call %d = %+v", i, r)
		}
	}
	conv := transcript.Rebuild(rows)
	h.sm.mu.Lock()
	last := h.sm.bodies[len(h.sm.bodies)-1]
	h.sm.mu.Unlock()
	saw, err := model.DecodeChatRequest(last)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []transcript.Message
	for _, turn := range conv.Turns {
		if turn.Reset {
			t.Fatalf("turn %d reset the conversation", turn.Step)
		}
		msgs = append(msgs[:turn.MessagesFrom], turn.Messages...)
	}
	got, _ := json.Marshal(msgs)
	want, _ := json.Marshal(transcript.Delta(transcript.State{}, saw, nil).Messages)
	if string(got) != string(want) {
		t.Fatalf("rebuilt conversation:\n%s\nthe provider was sent:\n%s", got, want)
	}
	if conv.System != saw.System || len(conv.Tools) != len(saw.Tools) || len(msgs) != 5 {
		t.Fatalf("system %.40q, %d tools, %d messages", conv.System, len(conv.Tools), len(msgs))
	}
	if calls := conv.Turns[2].Response.ToolCalls; len(calls) != 1 || calls[0].Name != "submit_review" ||
		!strings.Contains(string(calls[0].Input), "b is unused") {
		t.Fatalf("last response = %+v", conv.Turns[2].Response)
	}
	h.checkNoSecrets(t, `review_id = $1`, reviewID)
	if n := len(h.modelCalls(t, h.other.ID(), reviewID)); n != 0 {
		t.Fatalf("globex reads %d of acme's model calls", n)
	}
}

func (h *agenticHarness) modelCalls(t *testing.T, accountID, reviewID string) []transcript.StoredRow {
	t.Helper()
	var rows []transcript.StoredRow
	if err := h.st.WithAccount(h.ctx, accountID, func(tx pgx.Tx) error {
		var err error
		rows, err = store.ReviewModelCalls(h.ctx, tx, reviewID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// checkStepMasked checks that the run's recorded step kept its text with
// the run token and provider key in it masked.
func (h *agenticHarness) checkStepMasked(t *testing.T, runID string) {
	t.Helper()
	if text := h.checkNoSecrets(t, `runner_run_id = $1`, runID); !strings.Contains(text, "review with *** and ***") {
		t.Fatalf("the step's masked text is not recorded:\n%s", text)
	}
}

// checkNoSecrets fails when any column of the model calls where selects
// holds the provider's key or URL credentials, or a run token.
func (h *agenticHarness) checkNoSecrets(t *testing.T, where string, args ...any) string {
	t.Helper()
	var text string
	if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT coalesce(string_agg(to_jsonb(m)::text, ''), '') FROM model_calls m WHERE `+where, args...).Scan(&text)
	}); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"model-key", "provider-secret", "krk_"} {
		if strings.Contains(text, secret) {
			t.Fatalf("a model call holds %q:\n%s", secret, text)
		}
	}
	return text
}

func checkAgentRunsCommands(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptRun)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc v2() {}\n")
	h.dispatch(t, next)
	reviewID, status, errText := h.waitReview(t, next)
	if status != "completed" {
		t.Fatalf("status = %s (%s)", status, errText)
	}
	run := h.agentRow(t, reviewID)
	var tools map[string]int
	_ = json.Unmarshal([]byte(run.toolCalls), &tools)
	if run.stop != "submitted" || tools["run"] != 1 || run.sources != `["https://releases.example.com/b/v2"]` {
		t.Fatalf("agent run = %+v", run)
	}
	h.sm.mu.Lock()
	results, system := h.sm.toolResults, h.sm.systems[len(h.sm.systems)-1]
	h.sm.mu.Unlock()
	if len(results) == 0 || results[len(results)-1] != "exit code 0\nfetched -sS https://releases.example.com/b/v2\n" {
		t.Fatalf("tool results = %q", results)
	}
	if !strings.Contains(system, "run tool: curl.") {
		t.Fatalf("system prompt lacks the run tool:\n%s", system)
	}
	h.lf.mu.Lock()
	sticky := h.lf.comments[commentBase+1]
	h.lf.mu.Unlock()
	if !strings.Contains(sticky, "<summary>Sources consulted</summary>\n\n- <https://releases.example.com/b/v2>\n") {
		t.Fatalf("sticky:\n%s", sticky)
	}
}

// checkGatewayEndpoint calls the gateway the way a runner does, with a run
// token minted for a run of its own: steps are answered through the
// account's provider and charged, and refused once the budget is spent, for
// a model other than the run's, or without a valid token.
func checkGatewayEndpoint(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	args := jobs.ReviewArgs{AccountID: h.account.ID(), RepositoryID: configfile.RepositoryID(h.account.ID(), "acme/widgets"), Number: 1,
		HeadSHA: strings.Repeat("c", 40), Trigger: "test"}
	pr, err := loadPullRequest(h.ctx, h.st, args.AccountID, args.RepositoryID, args.Number)
	if err != nil {
		t.Fatal(err)
	}
	reviewID, runID, _, err := h.review.start(h.ctx, args, pr, h.base, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.st.MintGatewayToken(h.ctx, store.GatewayGrant{
		RunID: runID, AccountID: h.account.ID(), ReviewID: reviewID, RepositoryID: pr.repositoryID, Model: "gateway/agent-model", Budget: 150,
	}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	step := func(token, name string) (model.StepResponse, error) {
		c, err := model.NewOpenAI(model.OpenAIConfig{BaseURL: h.gatewayURL + "/v1", APIKey: token, ReportsModel: true})
		if err != nil {
			t.Fatal(err)
		}
		// Far more than one step may answer with.
		// The text carries the run's token and the provider's key, which the
		// transcript must mask.
		return c.Step(h.ctx, model.StepRequest{Model: name, Messages: []model.Message{{Role: model.RoleUser,
			Text: "review with " + token + " and model-key"}}, MaxTokens: 1 << 20})
	}
	resp, err := step(token, gateway.ModelName)
	if err != nil || resp.Model != "agent-model" || len(resp.ToolCalls) != 1 || resp.Usage.Prompt() != 100 || resp.CostUSD != 0.01 {
		t.Fatalf("step = %+v, %v", resp, err)
	}
	h.sm.mu.Lock()
	asked := h.sm.maxTokens[len(h.sm.maxTokens)-1]
	h.sm.mu.Unlock()
	if asked != gateway.MaxStepOutput {
		t.Fatalf("the provider was asked for %d tokens, want the step cap %d", asked, gateway.MaxStepOutput)
	}
	if grant, err := h.st.LookupGatewayToken(h.ctx, token); err != nil || grant.Spent != 110 {
		t.Fatalf("spent after a step = %d, %v; want the step's actual spend", grant.Spent, err)
	}
	if rows, tokens := h.usageTokens(t, reviewID); rows != 1 || tokens != 110 {
		t.Fatalf("usage rows=%d tokens=%d", rows, tokens)
	}
	h.checkStepMasked(t, runID)
	if _, err := step(token, "gpt-9-max"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a model other than the run's = %v", err)
	}
	for _, bad := range []string{"krk_", "krk_" + strings.Repeat("0", 64), "model-key"} {
		if _, err := step(bad, gateway.ModelName); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("token %q = %v", bad, err)
		}
	}
	// A second step is still within the budget; the third is not.
	if _, err := step(token, gateway.ModelName); err != nil {
		t.Fatal(err)
	}
	h.sm.mu.Lock()
	before := h.sm.requests
	h.sm.mu.Unlock()
	if _, err := step(token, gateway.ModelName); !errors.Is(err, model.ErrBudget) || !strings.Contains(err.Error(), "budget of 150 tokens") {
		t.Fatalf("a step past the budget = %v", err)
	}
	h.sm.mu.Lock()
	after := h.sm.requests
	h.sm.mu.Unlock()
	if after != before {
		t.Fatal("a refused step reached the provider")
	}
	checkParallelSteps(t, h, store.GatewayGrant{
		RunID: runID, AccountID: h.account.ID(), ReviewID: reviewID, RepositoryID: pr.repositoryID, Model: "gateway/agent-model",
	}, step)
	if err := h.st.RevokeGatewayTokens(h.ctx, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := step(token, gateway.ModelName); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a revoked token = %v", err)
	}
	// The suites share one database; count only this account's tokens.
	var left int
	if err := h.st.App().QueryRow(h.ctx, `SELECT count(*) FROM gateway_tokens WHERE account_id = $1`, h.account.ID()).Scan(&left); err != nil ||
		left != 0 {
		t.Fatalf("gateway tokens left after the reviews = %d, %v", left, err)
	}
	if err := failRun(h.ctx, h.st, h.account.ID(), runID, "test run"); err != nil {
		t.Fatal(err)
	}
}

// checkParallelSteps sends five steps at once on a token whose budget any
// one step spends. Each reserves before it runs, so one is served and the
// rest are refused, however they interleave.
func checkParallelSteps(t *testing.T, h *agenticHarness, grant store.GatewayGrant, step func(token, name string) (model.StepResponse, error)) {
	t.Helper()
	grant.Budget = 100
	token, err := h.st.MintGatewayToken(h.ctx, grant, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h.sm.mu.Lock()
	before := h.sm.requests
	h.sm.mu.Unlock()
	errs := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := step(token, gateway.ModelName)
			errs <- err
		}()
	}
	var served, refused int
	for range 5 {
		switch err := <-errs; {
		case err == nil:
			served++
		case errors.Is(err, model.ErrBudget):
			refused++
		default:
			t.Fatalf("parallel step = %v", err)
		}
	}
	h.sm.mu.Lock()
	after := h.sm.requests
	h.sm.mu.Unlock()
	if served != 1 || refused != 4 || after-before != 1 {
		t.Fatalf("served %d, refused %d, provider called %d times", served, refused, after-before)
	}
}

// checkAgentSnoozes holds the account's one model slot and dispatches a new
// head: the agentic review is snoozed without a review row, a lease or a
// runner until the slot is let go, and then runs to completion.
func checkAgentSnoozes(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc snoozed() {}\n")
	hold := func(jobID *int64) {
		t.Helper()
		if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(h.ctx, `INSERT INTO model_leases (account_id, model_key, slot, job_id, expires_at)
				VALUES ($1, 'gateway/agent-model', 1, $2, CASE WHEN $2::bigint IS NULL THEN NULL ELSE now() + interval '1 hour' END)
				ON CONFLICT (account_id, model_key, slot) DO UPDATE SET job_id = excluded.job_id, expires_at = excluded.expires_at`,
				h.account.ID(), jobID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	holder := int64(-1)
	hold(&holder)
	h.dispatch(t, next)
	waitFor(t, 30*time.Second, "the agentic review to snooze", func() bool {
		var n int
		if err := h.st.App().QueryRow(h.ctx, `SELECT coalesce(max((metadata->>'snoozes')::int), 0) FROM river_job
			WHERE kind = 'review' AND args->>'head_sha' = $1`, next).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n >= 1
	})
	var reviews int
	if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM reviews WHERE head_sha = $1`, next).Scan(&reviews)
	}); err != nil || reviews != 0 {
		t.Fatalf("a snoozed agentic review recorded %d reviews, %v", reviews, err)
	}
	hold(nil)
	if _, status, errText := h.waitReview(t, next); status != "completed" {
		t.Fatalf("status = %s (%s), want completed once the slot was free", status, errText)
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
	if rows, tokens := h.usageTokens(t, reviewID); rows != 2 || tokens != 220 {
		t.Fatalf("an unsubmitted run still spent tokens: usage rows=%d tokens=%d", rows, tokens)
	}
	h.lf.mu.Lock()
	comments, sticky, forgeStatus := len(h.lf.comments), h.lf.comments[commentBase+1], h.lf.status
	h.lf.mu.Unlock()
	if comments != 1 || !strings.Contains(sticky, "**Review incomplete for [`"+next[:7]+"`](local://acme/widgets/commit/"+next+"):** agent stopped: no_submit.") ||
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
	// tools are the names of the tools the last run was handed.
	tools []string
}

func (e *hookExecutor) Run(ctx context.Context, spec executor.Spec) executor.Result {
	e.mu.Lock()
	hold, detach := e.hold, e.detach
	e.hold, e.detach = 0, false
	e.tools = nil
	for _, tool := range spec.Tools {
		e.tools = append(e.tools, tool.Name)
	}
	e.mu.Unlock()
	if detach {
		return e.runDetached(ctx, spec)
	}
	select {
	case <-time.After(hold):
	case <-ctx.Done():
		return executor.Result{JobName: "kritika-run-held", Err: context.Cause(ctx)}
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
		return executor.Result{JobName: "kritika-run-deleted", Err: cause}
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
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
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
		err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
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
	if rows, tokens := h.usageTokens(t, reviewID); rows != 3 || tokens != 330 {
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
		strings.Contains(run.errText, "model-key") || strings.Contains(errText, "model-key") || !strings.Contains(errText, "***") ||
		strings.Contains(run.errText, "provider-secret") || strings.Contains(errText, "provider-secret") {
		t.Fatalf("status=%s review error=%q agent error=%q", status, errText, run.errText)
	}
	// No step was answered, so the run names no model and the review the
	// one it was granted, never the gateway's name for it.
	var reviewModel string
	if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT model FROM reviews WHERE id = $1`, reviewID).Scan(&reviewModel)
	}); err != nil || run.model != "" || reviewModel != "agent-model" {
		t.Fatalf("agent run model = %q, review model = %q, %v", run.model, reviewModel, err)
	}
	h.lf.mu.Lock()
	sticky := h.lf.comments[commentBase+1]
	h.lf.mu.Unlock()
	if !strings.Contains(sticky, "by kritika with agent-model.") {
		t.Fatalf("sticky:\n%s", sticky)
	}
	// The provider refused the step, so nothing was spent.
	if rows, _ := h.usageTokens(t, reviewID); rows != 0 {
		t.Fatalf("usage rows = %d", rows)
	}
	// The refused step is still recorded, the key it echoed masked.
	rows := h.modelCalls(t, h.account.ID(), reviewID)
	if len(rows) == 0 || !strings.Contains(rows[0].Error, "invalid api key ***") {
		t.Fatalf("model calls = %+v", rows)
	}
	h.checkNoSecrets(t, `review_id = $1`, reviewID)
}

// checkAgentRules checks the runner was given the rules whose whenExpr
// the worker found true of the pull request, and only those.
func checkAgentRules(t *testing.T, system string) {
	t.Helper()
	if !strings.Contains(system, "\n- into-main: Keep main releasable.") || strings.Contains(system, "- renovate:") {
		t.Fatalf("the system prompt has the wrong rules:\n%s", system)
	}
}

func checkAgentFiltered(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	h.sm.mu.Lock()
	before := h.sm.requests
	h.sm.mu.Unlock()
	base := h.commit(t, ".kritika.yaml", "filterExpr: '!pr.body.contains(\"[skip-review]\")'\n")
	h.lf.setBase(base)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc f() {}\n")
	h.lf.mu.Lock()
	h.lf.status = ""
	h.lf.mu.Unlock()
	h.dispatchBody(t, next, "Adds f. [skip-review]")
	reviewID, status, _ := h.waitReview(t, next)
	if status != "skipped" {
		t.Fatalf("status = %s, want skipped", status)
	}
	var reason string
	var runs int
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT skip_reason, (SELECT count(*) FROM runner_runs WHERE review_id = $1) FROM reviews WHERE id = $1`,
			reviewID).Scan(&reason, &runs)
	})
	if err != nil || reason != "filtered" || runs != 0 {
		t.Fatalf("skip reason %q with %d runner run(s), %v; want filtered with none", reason, runs, err)
	}
	h.sm.mu.Lock()
	after := h.sm.requests
	h.sm.mu.Unlock()
	if rows, _ := h.usageTokens(t, reviewID); after != before || rows != 0 {
		t.Fatalf("a filtered review called the model %d time(s) and has %d usage row(s)", after-before, rows)
	}
	h.lf.mu.Lock()
	forgeStatus := h.lf.status
	h.lf.mu.Unlock()
	if forgeStatus != "success: kritika: skipped (filtered by .kritika.yaml)" {
		t.Fatalf("forge status = %q", forgeStatus)
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
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*) FROM reviews WHERE head_sha = $1`, next).Scan(&reviews)
	})
	if err != nil || reviews != 1 {
		t.Fatalf("the review was cut off and retried: %d review rows, err=%v", reviews, err)
	}
}

// checkAgentCappedUnderLease caps a review on the account's daily count,
// which earlier subtests have already spent, and checks the lease taken to
// read the caps is released rather than held for the capped review.
func checkAgentCappedUnderLease(t *testing.T, h *agenticHarness) {
	capped := *h.file
	capped.Defaults.Limits.ReviewsPerDay = new(1)
	h.review.Current.Set(&capped)
	t.Cleanup(func() { h.review.Current.Set(h.file) })
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc capped() {}\n")
	h.dispatch(t, next)
	_, status, errText := h.waitReview(t, next)
	if status != string(store.ReviewCapped) || !strings.Contains(errText, "reviewsPerDay") {
		t.Fatalf("status = %s (%s), want capped on reviewsPerDay", status, errText)
	}
	var held int
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
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
		err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
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
	args := jobs.ReviewArgs{AccountID: h.account.ID(), RepositoryID: configfile.RepositoryID(h.account.ID(), "acme/widgets"), Number: 1,
		HeadSHA: strings.Repeat("d", 40), Trigger: "test"}
	pr, err := loadPullRequest(h.ctx, h.st, args.AccountID, args.RepositoryID, args.Number)
	if err != nil {
		t.Fatal(err)
	}
	_, runID, _, err := h.review.start(h.ctx, args, pr, h.base, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := failRun(h.ctx, h.st, h.account.ID(), runID, "worker: read pull request for the filter: boom"); err != nil {
		t.Fatal(err)
	}
	var phase, errText string
	var finished bool
	err = h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT phase, error, finished_at IS NOT NULL FROM runner_runs WHERE id = $1`, runID).
			Scan(&phase, &errText, &finished)
	})
	if err != nil || phase != "failed" || !strings.Contains(errText, "boom") || !finished {
		t.Fatalf("run phase=%q error=%q finished=%v err=%v", phase, errText, finished, err)
	}
}

func checkAgentSpecFailed(t *testing.T, h *agenticHarness) {
	boom := errors.New("worker: mint gateway token: boom")
	remote, cancelRemote := context.WithCancelCause(h.ctx)
	cancelRemote(river.ErrJobCancelledRemotely)
	timedOut, cancelTimeout := context.WithCancelCause(h.ctx)
	cancelTimeout(context.DeadlineExceeded)
	// River cancels a stopping client's jobs with an error of its own.
	stopped, cancelStop := context.WithCancelCause(h.ctx)
	cancelStop(errors.New("stop initiated"))
	tests := []struct {
		name      string
		ctx       context.Context
		head      string
		retried   bool
		status    store.ReviewStatus
		errPrefix string
	}{
		{"the job still runs: River retries", h.ctx, strings.Repeat("e", 40), true, store.ReviewFailed, boom.Error()},
		{"a remote cancel ends it canceled", remote, strings.Repeat("f", 40), false, store.ReviewCanceled, ""},
		{"a timeout ends it failed", timedOut, strings.Repeat("9", 40), false, store.ReviewFailed, "review timed out"},
		{"a stopping worker's cut is retried", stopped, strings.Repeat("8", 40), true, store.ReviewSuperseded, "cut by a restart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := jobs.ReviewArgs{AccountID: h.account.ID(), RepositoryID: configfile.RepositoryID(h.account.ID(), "acme/widgets"),
				Number: 1, HeadSHA: tt.head, Trigger: "test"}
			pr, err := loadPullRequest(h.ctx, h.st, args.AccountID, args.RepositoryID, args.Number)
			if err != nil {
				t.Fatal(err)
			}
			reviewID, runID, _, err := h.review.start(h.ctx, args, pr, h.base, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			e := endedReview{accountID: h.account.ID(), accountKey: h.account.Key(), reviewID: reviewID, headSHA: tt.head,
				owner: "acme", repo: "widgets", client: h.lf, started: time.Now(), logger: slog.New(slog.DiscardHandler)}
			err = h.review.agentSpecFailed(tt.ctx, e, runID, boom)
			if (err != nil) != tt.retried {
				t.Fatalf("agentSpecFailed = %v, want an error only when River should retry", err)
			}
			var status, errText, phase string
			err = h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
				return tx.QueryRow(h.ctx, `SELECT r.status, coalesce(r.error, ''), rr.phase FROM reviews r, runner_runs rr
					WHERE r.id = $1 AND rr.id = $2`, reviewID, runID).Scan(&status, &errText, &phase)
			})
			if err != nil || status != string(tt.status) || !strings.HasPrefix(errText, tt.errPrefix) || phase != "failed" {
				t.Fatalf("review %s (%q), run %s, err %v; want review %s (%q...), run failed", status, errText, phase, err, tt.status, tt.errPrefix)
			}
		})
	}
}

// checkRunnerOnlySkip has the runner skip a bot's rebase as unchanged
// while the worker, reading the last review again after the run, does not:
// the review the runner matched changes as its Job ends.
func checkRunnerOnlySkip(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc k() {}\n")
	base, _ := h.lf.MergeBase(h.ctx, "", "", "", "")
	fetched, err := gitfetch.Run(h.ctx, gitfetch.Fetch{CloneURL: h.dir, Head: next, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	patch := fetched.PatchID
	_ = fetched.Close()
	// The last review had this head's patch, but the forge reported another
	// patch for it, so the worker's own check before the runner passes.
	var lastID string
	err = h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `INSERT INTO reviews (account_id, pull_request_id, head_sha, merge_base_sha, patch_id, forge_patch_id,
			status, trigger, finished_at) SELECT $1, id, $2, $3, $4, 'another', 'completed', 'synchronize', now()
			FROM pull_requests WHERE number = 1 RETURNING id`, h.account.ID(), h.head, base, patch).Scan(&lastID)
	})
	if err != nil {
		t.Fatal(err)
	}
	h.lf.mu.Lock()
	h.lf.status = ""
	h.lf.mu.Unlock()
	h.exec.mu.Lock()
	h.exec.after = func() {
		err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
			_, err := tx.Exec(h.ctx, `UPDATE reviews SET patch_id = 'changed' WHERE id = $1`, lastID)
			return err
		})
		if err != nil {
			t.Error(err)
		}
	}
	h.exec.mu.Unlock()
	h.dispatchAs(t, next, "Adds k.", true)
	reviewID, status, _ := h.waitReview(t, next)
	if status != "skipped" {
		t.Fatalf("status = %s, want skipped", status)
	}
	var skip string
	var agentRows int
	err = h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT c.skip_reason, (SELECT count(*) FROM agent_runs a WHERE a.runner_run_id = r.id)
			FROM runner_runs r JOIN context_packs c ON c.runner_run_id = r.id WHERE r.review_id = $1`, reviewID).Scan(&skip, &agentRows)
	})
	if err != nil || skip != runner.SkipUnchangedPatch || agentRows != 0 {
		t.Fatalf("pack skip = %q with %d agent run(s), %v; want %s with none", skip, agentRows, err, runner.SkipUnchangedPatch)
	}
	h.lf.mu.Lock()
	forgeStatus := h.lf.status
	h.lf.mu.Unlock()
	if forgeStatus != "success: kritika: skipped (patch unchanged since the last review)" {
		t.Fatalf("forge status = %q", forgeStatus)
	}
}

// checkTooLarge sets maxChangedLines under what a push changes: the runner
// skips the review before any model call and the commit status says so,
// while a review someone asks for runs the agent anyway.
func checkTooLarge(t *testing.T, h *agenticHarness) {
	h.sm.reset(scriptSubmit)
	h.sm.mu.Lock()
	before := h.sm.requests
	h.sm.mu.Unlock()
	limited := *h.file
	limited.Defaults.MaxChangedLines = new(2)
	h.review.Current.Set(&limited)
	t.Cleanup(func() { h.review.Current.Set(h.file) })
	next := h.commit(t, "main.go", "package main\n\nfunc b() {}\n\nfunc large() {}\n\nfunc larger() {}\n")
	h.lf.mu.Lock()
	h.lf.status = ""
	h.lf.mu.Unlock()
	h.dispatch(t, next)
	reviewID, status, _ := h.waitReview(t, next)
	if status != "skipped" {
		t.Fatalf("status = %s, want skipped", status)
	}
	var skip string
	var agentRows int
	err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT c.skip_reason, (SELECT count(*) FROM agent_runs a WHERE a.runner_run_id = r.id)
			FROM runner_runs r JOIN context_packs c ON c.runner_run_id = r.id WHERE r.review_id = $1`, reviewID).Scan(&skip, &agentRows)
	})
	if err != nil || skip != runner.SkipTooLarge || agentRows != 0 {
		t.Fatalf("pack skip = %q with %d agent run(s), %v; want %s with none", skip, agentRows, err, runner.SkipTooLarge)
	}
	h.sm.mu.Lock()
	after := h.sm.requests
	h.sm.mu.Unlock()
	if after != before {
		t.Fatalf("a skipped review called the model %d time(s)", after-before)
	}
	h.lf.mu.Lock()
	forgeStatus := h.lf.status
	h.lf.mu.Unlock()
	if forgeStatus != "success: kritika: skipped (more than 2 changed lines)" {
		t.Fatalf("forge status = %q", forgeStatus)
	}

	// Asked for, as "@acme-bot review" does, the same head is reviewed
	// whatever its size. The skipped review is already finished, so the
	// second finished one is waited for. Its job settles after the review
	// row does, and a re-run is refused while the job is still live.
	waitRiverJobCompleted(h.ctx, t, h.st, h.account.ID(), latestReviewID(h.ctx, t, h.st, h.account.ID(), next))
	err = h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		_, err := jobs.EnqueueRerun(h.ctx, tx, h.insertOnly, h.account.ID(), configfile.RepositoryID(h.account.ID(), "acme/widgets"), 1)
		return err
	})
	if err != nil {
		t.Fatalf("EnqueueRerun: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var finished int
		err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(h.ctx, `SELECT count(*), coalesce((SELECT status FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL
				ORDER BY created_at DESC LIMIT 1), '') FROM reviews WHERE head_sha = $1 AND finished_at IS NOT NULL`, next).Scan(&finished, &status)
		})
		if err != nil {
			t.Fatal(err)
		}
		if finished >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no second finished review for %s", next)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != "completed" {
		t.Fatalf("status = %s, want completed: a review someone asked for is never too large", status)
	}
}

// checkGatewaySimilar indexes acme/widgets and checks stage 4 through the
// gateway: the runner asks for it before its prompt, the gateway answers
// from the index outside the changed paths and charges the embedding to
// the run, and refuses a run without a pack or past its budget.
func checkGatewaySimilar(t *testing.T, h *agenticHarness) {
	withIndex, err := configfiletest.Parse(t, h.config+"embedding: { model: gateway/fake-embed, dims: 8 }\n")
	if err != nil {
		t.Fatal(err)
	}
	h.review.Current.Set(withIndex)
	t.Cleanup(func() { h.review.Current.Set(h.file) })
	repoID := configfile.RepositoryID(h.account.ID(), "acme/widgets")
	code := "func b() {}\n\nfunc c() {}\n"
	seedIndex(t, h, withIndex.Embedding, repoID, code)

	h.sm.reset(scriptSubmit)
	h.sm.mu.Lock()
	first := len(h.sm.bodies)
	h.sm.mu.Unlock()
	next := h.commit(t, "main.go", "package main\n\n"+code)
	h.dispatch(t, next)
	reviewID, status, errText := h.waitReview(t, next)
	if status != "completed" {
		t.Fatalf("status = %s (%s)", status, errText)
	}
	h.sm.mu.Lock()
	prompt := string(h.sm.bodies[first])
	h.sm.mu.Unlock()
	if !strings.Contains(prompt, "### similar: other.go") || strings.Contains(prompt, "### similar: main.go") ||
		!strings.Contains(prompt, `"name":"search_code"`) || !strings.Contains(prompt, "search the repository by meaning") {
		t.Fatalf("stage 4 or search_code missing or wrong in the agent's first request:\n%s", prompt)
	}
	var stages string
	if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT c.stages::text FROM context_packs c JOIN runner_runs r ON r.id = c.runner_run_id
			WHERE r.review_id = $1`, reviewID).Scan(&stages)
	}); err != nil || !strings.Contains(stages, `"stage": "similar"`) || !strings.Contains(stages, `"path": "other.go"`) {
		t.Fatalf("the pack lacks stage 4: %v\n%s", err, stages)
	}
	var embedded int
	var tokens int64
	if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `SELECT count(*), coalesce(sum(input_tokens), 0) FROM usage WHERE review_id = $1 AND role = 'embedding'`,
			reviewID).Scan(&embedded, &tokens)
	}); err != nil || embedded != 1 || tokens <= 0 {
		t.Fatalf("embedding usage rows=%d tokens=%d err=%v", embedded, tokens, err)
	}

	checkSimilarRoute(t, h, repoID, code)
}

// seedIndex makes an active index generation for repoID holding code at
// other.go and at main.go, which the pull request changes.
func seedIndex(t *testing.T, h *agenticHarness, emb *configfile.Embedding, repoID, code string) {
	t.Helper()
	if _, err := h.st.EnsureIndexSchema(h.ctx, "kritika_app", emb.Model, emb.Dims); err != nil {
		t.Fatal(err)
	}
	vectors, _, err := h.fe.Embed(h.ctx, []string{code})
	if err != nil {
		t.Fatal(err)
	}
	err = h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		var runID string
		if err := tx.QueryRow(h.ctx, `INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
			VALUES ($1, $2, $3, $4, $5, 'full', 'completed') RETURNING id`, h.account.ID(), repoID, h.base, emb.Model, emb.Dims).Scan(&runID); err != nil {
			return err
		}
		// main.go is changed by the pull request, so the overlay has it and
		// stage 4 must not repeat it.
		for _, path := range []string{"other.go", "main.go"} {
			if _, err := tx.Exec(h.ctx, `INSERT INTO index_chunks (account_id, repository_id, index_run_id, path, start_line, end_line, text, embedding)
				VALUES ($1, $2, $3, $4, 1, 3, $5, $6::halfvec)`, h.account.ID(), repoID, runID, path, code, model.VectorLiteral(vectors[0])); err != nil {
				return err
			}
		}
		_, err := tx.Exec(h.ctx, `UPDATE repositories SET active_index_run_id = $2 WHERE id = $1`, repoID, runID)
		return err
	})
	if err != nil {
		t.Fatalf("seed the index: %v", err)
	}
}

// checkSimilarRoute calls /v1/similar the way a runner does: refused
// without a valid token or without queries, answered from the index, and
// refused once the run's budget is spent.
func checkSimilarRoute(t *testing.T, h *agenticHarness, repoID, code string) {
	t.Helper()
	query := "main.go\n" + code
	similar := func(token string, body string) (int, contextpack.SimilarResponse) {
		t.Helper()
		req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.gatewayURL+"/v1/similar", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out contextpack.SimilarResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	encode := func(req contextpack.SimilarRequest) string {
		b, _ := json.Marshal(req)
		return string(b)
	}
	if code, _ := similar("krk_"+strings.Repeat("0", 64), encode(contextpack.SimilarRequest{Queries: []string{query}})); code != http.StatusUnauthorized {
		t.Fatalf("an unknown token = %d", code)
	}
	args := jobs.ReviewArgs{AccountID: h.account.ID(), RepositoryID: repoID, Number: 1, HeadSHA: strings.Repeat("d", 40), Trigger: "test"}
	pr, err := loadPullRequest(h.ctx, h.st, args.AccountID, args.RepositoryID, args.Number)
	if err != nil {
		t.Fatal(err)
	}
	reviewID, runID, _, err := h.review.start(h.ctx, args, pr, h.base, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// A budget the first call's embedding overruns: it is reserved while
	// the run has spent nothing, and the second finds it spent.
	token, err := h.st.MintGatewayToken(h.ctx, store.GatewayGrant{
		RunID: runID, AccountID: h.account.ID(), ReviewID: reviewID, RepositoryID: repoID, Model: "gateway/agent-model", Budget: 1,
	}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"no queries": `{"queries":[]}`, "an empty query": `{"queries":[" "]}`, "not json": `{`,
		"too many queries": encode(contextpack.SimilarRequest{Queries: slices.Repeat([]string{query}, contextpack.SimilarQueries+1)}),
	} {
		if code, _ := similar(token, body); code != http.StatusBadRequest {
			t.Fatalf("%s = %d", name, code)
		}
	}
	code200, out := similar(token, encode(contextpack.SimilarRequest{Queries: []string{query}, Exclude: []string{"main.go"}}))
	if code200 != http.StatusOK || !out.Indexed || len(out.Chunks) != 1 || out.Chunks[0].Path != "other.go" || out.Chunks[0].Stage != contextpack.StageSimilar {
		t.Fatalf("similar = %d %+v", code200, out)
	}
	grant, err := h.st.LookupGatewayToken(h.ctx, token)
	if err != nil || grant.Spent <= 0 || grant.Spent >= int64(len(query))/4+1 {
		t.Fatalf("spent = %d, %v; want the embedding's tokens, not the reservation", grant.Spent, err)
	}
	if code, _ := similar(token, encode(contextpack.SimilarRequest{Queries: []string{query}})); code != http.StatusTooManyRequests {
		t.Fatalf("a call past the run's budget = %d", code)
	}
	// A repository without an index answers indexed false, so the runner
	// offers no search tool.
	otherRepo := configfile.RepositoryID(h.account.ID(), "acme/gadgets")
	if err := h.st.WithAccount(h.ctx, h.account.ID(), func(tx pgx.Tx) error {
		_, err := tx.Exec(h.ctx, `INSERT INTO repositories (id, account_id, name, managed_by) VALUES ($1, $2, 'acme/gadgets', 'forge')
			ON CONFLICT DO NOTHING`, otherRepo, h.account.ID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	token2, err := h.st.MintGatewayToken(h.ctx, store.GatewayGrant{
		RunID: runID, AccountID: h.account.ID(), ReviewID: reviewID, RepositoryID: otherRepo, Model: "gateway/agent-model", Budget: 1000,
	}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if code, out := similar(token2, encode(contextpack.SimilarRequest{Queries: []string{query}})); code != http.StatusOK || out.Indexed || len(out.Chunks) != 0 {
		t.Fatalf("an unindexed repository = %d %+v", code, out)
	}
	if err := h.st.RevokeGatewayTokens(h.ctx, runID); err != nil {
		t.Fatal(err)
	}
}
