package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/agent"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/contextpack"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

const agentDiff = `diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -1,1 +1,3 @@
 package main
+
+func b() {}
`

func agentPromptSpec() Spec {
	s := reviewSpec()
	s.PriorHead = shaB
	return s
}

func TestAgentPrompt(t *testing.T) {
	pack := packView{
		Diff: agentDiff, Changed: []string{"main.go"},
		Context: []contextpack.Chunk{{Stage: contextpack.StageDefinition, Path: "util.go", StartLine: 1, EndLine: 2, Text: "func u() {}"}},
		Scope:   review.ScopeFull,
	}
	files := repoconfig.Files{"docs/rules.md": "Admin rules.", ".kritik/rules.md": "Repository rules.", "AGENTS.md": "Agent notes."}
	tests := []struct {
		name         string
		scope        review.Scope
		instructions []string
		rules        []configfile.Rule
		active       []review.Rule
		strict       bool
		focused      bool
		agentFiles   bool
	}{
		{name: "strictness", scope: review.ScopeFull, strict: true},
		{name: "the root's AGENTS.md is the instructions", agentFiles: true,
			scope: review.ScopeFull, instructions: []string{"Agent notes."}, strict: true},
		{name: "incremental adds the delta and the prior findings", scope: review.ScopeIncremental, strict: true},
		{name: "a focused review gets the focused prompt", scope: review.ScopeFull, focused: true},
		{
			name: "a rule scoped to paths the change does not touch is left out", scope: review.ScopeFull,
			rules:  []configfile.Rule{{ID: "go", Rule: "Wrap errors.", Paths: []string{"*.go"}}, {ID: "web", Rule: "No inline styles.", Paths: []string{"web/**"}}},
			active: []review.Rule{{ID: "go", Text: "Wrap errors."}},
		},
		{
			name: "a file rule carries its file, in the order written", scope: review.ScopeFull,
			rules: []configfile.Rule{{ID: "repo", File: ".kritik/rules.md"}, {ID: "admin", File: "docs/rules.md", Paths: []string{"web/**"}},
				{ID: "go", Rule: "Wrap errors."}},
			active: []review.Rule{{ID: "repo", Text: "Repository rules.", File: ".kritik/rules.md"}, {ID: "go", Text: "Wrap errors."}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentPromptSpec()
			s.Prompt.RequireSuggestedFix, s.Prompt.Focused, s.Prompt.Rules, s.AgentFiles = tt.strict, tt.focused, tt.rules, tt.agentFiles
			pack := pack
			pack.Scope = tt.scope
			if tt.scope == review.ScopeIncremental {
				pack.DeltaDiff = agentDiff
			}
			in := newPromptInputs(s, files, pack.Changed)
			if !slices.Equal(in.ruleIDs(), ruleIDs(tt.active)) {
				t.Fatalf("rule ids = %v, want %v", in.ruleIDs(), ruleIDs(tt.active))
			}
			prompt := newAgentPrompt(s, in, pack, nil, false)
			system, user, strict := prompt.system, prompt.user, prompt.strict
			if want := review.SystemPrompt(tt.active, tt.instructions, nil, tt.focused, false); system != want {
				t.Fatalf("system prompt:\n%s", system)
			}
			var inc *review.IncrementalInput
			if tt.scope == review.ScopeIncremental {
				inc = &review.IncrementalInput{PriorHeadSHA: shaB, DeltaDiff: agentDiff, Prior: s.Prompt.Prior}
			}
			want, _, _ := review.Build(review.Input{
				Repository: "acme/widgets", Number: 7, Title: "Add b", Author: "octocat", Body: "Adds b.", BaseRef: "main",
				Changed: pack.Changed, Diff: agentDiff, Context: pack.Context, Incremental: inc, BudgetTokens: review.UserBudget(system),
			})
			if user != want {
				t.Fatalf("user message:\n%s\nwant:\n%s", user, want)
			}
			if strict != tt.strict {
				t.Fatalf("strict = %v, want %v", strict, tt.strict)
			}
			if tt.scope == review.ScopeIncremental && !strings.Contains(user, "earlier finding") {
				t.Fatalf("incremental prompt lacks the prior findings:\n%s", user)
			}
		})
	}
}

func TestAgentPromptPointsAtContext(t *testing.T) {
	s := agentPromptSpec()
	s.Prompt.Context = []configfile.ContextFile{
		{Path: "docs/arch.md", Description: "how the parts fit"},
		{Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}},
	}
	files := repoconfig.Files{"docs/arch.md": "never inlined"}
	pack := packView{Diff: agentDiff, Changed: []string{"main.go"}}
	user := newAgentPrompt(s, newPromptInputs(s, files, pack.Changed), pack, nil, false).user
	if !strings.Contains(user, "### docs/arch.md: how the parts fit\n") || strings.Contains(user, "never inlined") || strings.Contains(user, "schema") {
		t.Fatalf("user message:\n%s", user)
	}
}

// ruleIDs is the ids of rules, for comparing with a prompt's inputs.
func ruleIDs(rules []review.Rule) []string {
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestPromptInputsNotes: the notes say what the prompt's budgets cut.
func TestPromptInputsNotes(t *testing.T) {
	s := agentPromptSpec()
	s.AgentFiles = true
	s.Prompt.Rules = []configfile.Rule{{ID: "big", Rule: strings.Repeat("x", repoconfig.MaxRulesBytes)}, {ID: "left", Rule: "Wrap errors."}}
	files := repoconfig.Files{"AGENTS.md": strings.Repeat("y", repoconfig.MaxInstructionBytes+1)}
	in := newPromptInputs(s, files, []string{"main.go"})
	if want := []string{noteInstructionsTruncated, "1 review rules left out, past the 16 KiB of rule text or 32 KiB of rule files a review is given"}; !slices.Equal(in.notes, want) {
		t.Fatalf("notes = %q, want %q", in.notes, want)
	}
	const lines = 60_000
	big := "diff --git a/big.go b/big.go\n--- a/big.go\n+++ b/big.go\n@@ -0,0 +1," + strconv.Itoa(lines) + " @@\n" + strings.Repeat("+x\n", lines)
	long := packView{Diff: agentDiff + big, Changed: []string{"main.go", "big.go"}}
	prompt := newAgentPrompt(s, in, long, nil, false)
	if notes := prompt.notes(); len(notes) != 1 || notes[0] != "1 diff file(s) left out of the prompt to fit its budget: big.go" {
		t.Fatalf("prompt notes = %q", notes)
	}
	many := agentPrompt{omitted: []string{"a", "b", "c", "d", "e", "f", "g"}, contextOmitted: 2}
	want := []string{"7 diff file(s) left out of the prompt to fit its budget: a, b, c, d, e, and 2 more", "2 context chunk(s) left out of the prompt to fit its budget"}
	if notes := many.notes(); !slices.Equal(notes, want) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
}

func TestAgentSkip(t *testing.T) {
	tests := []struct {
		name    string
		ignore  []string
		changed []string
		patchID string
		want    string
	}{
		{name: "reviewed", changed: []string{"main.go"}, patchID: "p2"},
		{name: "unchanged bot patch", changed: []string{"main.go"}, patchID: "p1", want: SkipUnchangedPatch},
		{name: "only ignored paths", ignore: []string{"**/*.md"}, changed: []string{"docs/a.md"}, patchID: "p2",
			want: string(repoconfig.SkipOnlyPaths)},
		{name: "a path the ignore globs do not cover", ignore: []string{"**/*.md"}, changed: []string{"docs/a.md", "main.go"}, patchID: "p2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentPromptSpec()
			s.Prompt.UnchangedPatchID, s.Ignore = "p1", tt.ignore
			if got := agentSkip(s, tt.changed, tt.patchID); got != tt.want {
				t.Fatalf("agentSkip = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentErrorsAreMasked(t *testing.T) {
	secrets := Secrets{GitToken: "git-token", GatewayToken: "krk_run_token"}
	rec, err := newAgentRecord(agent.Result{Stop: agent.StopError, Err: `401: {"error":"bad token krk_run_token"}`},
		nil, []string{"https://example.com/?key=krk_run_token"}, secrets)
	if err != nil || strings.Contains(rec.err, "krk_run_token") || !strings.Contains(rec.err, "bad token ***") {
		t.Fatalf("agent run error = %q, %v", rec.err, err)
	}
	if string(rec.sources) != `["https://example.com/?key=***"]` {
		t.Fatalf("sources = %s", rec.sources)
	}
	if rec, err := newAgentRecord(agent.Result{Stop: agent.StopMaxSteps}, nil, nil, secrets); err != nil || string(rec.sources) != "[]" {
		t.Fatalf("sources of a run without commands = %s, %v", rec.sources, err)
	}
	if got := secrets.Mask("clone https://x:git-token@forge.example.com: denied"); strings.Contains(got, "git-token") {
		t.Fatalf("run error = %q", got)
	}
}

// scriptedStepper answers each step with the next scripted response.
type scriptedStepper struct {
	mu    sync.Mutex
	steps []model.StepResponse
	reqs  []model.StepRequest
}

func (s *scriptedStepper) Step(_ context.Context, req model.StepRequest) (model.StepResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if len(s.reqs) > len(s.steps) {
		return model.StepResponse{Text: "done"}, nil
	}
	return s.steps[len(s.reqs)-1], nil
}

func call(id, name, input string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

func TestReviewAgentRecordsATimeline(t *testing.T) {
	head := tree(t, map[string]string{"main.go": "package main\n\nfunc b() {}\n", "vendor/x.go": "func b() {}\n"})
	st := &scriptedStepper{steps: []model.StepResponse{
		{ToolCalls: []model.ToolCall{call("1", "grep", `{"pattern":"func b"}`)}, Usage: model.Usage{Input: 100, Output: 10}},
		{ToolCalls: []model.ToolCall{call("2", "read_file", `{"path":"main.go"}`)}, Usage: model.Usage{Input: 200, CacheRead: 50, Output: 20}},
		{ToolCalls: []model.ToolCall{call("3", "submit_review", `{"summary":{"take":"ok","praise":[]},"findings":[]}`)},
			Usage: model.Usage{Input: 300, Output: 30}, CostUSD: 0.5},
	}}
	s := agentPromptSpec()
	logger := slog.New(slog.DiscardHandler)
	res, timeline := reviewAgent(t.Context(), st, s, head, []string{"vendor/**"}, nil, "system", "user", true, time.Minute, logger)
	if res.Stop != agent.StopSubmitted || res.Steps != 3 || res.ToolCalls["grep"] != 1 || res.ToolCalls["read_file"] != 1 ||
		res.ToolCalls["submit_review"] != 1 || res.CostUSD != 0.5 {
		t.Fatalf("result = %+v", res)
	}
	if len(timeline) != 3 {
		t.Fatalf("timeline = %+v", timeline)
	}
	for i, want := range []struct {
		tool          string
		input, output int64
	}{{"grep", 100, 10}, {"read_file", 250, 20}, {"submit_review", 300, 30}} {
		step := timeline[i]
		if step.Index != i || !slices.Equal(step.Tools, []string{want.tool}) || step.InputTokens != want.input ||
			step.OutputTokens != want.output || step.DurationMS < 0 {
			t.Fatalf("step %d = %+v", i, step)
		}
	}
	if timeline[0].OutputBytes == 0 || !strings.Contains(string(mustJSON(t, timeline)), `"duration_ms"`) {
		t.Fatalf("timeline = %s", mustJSON(t, timeline))
	}
	// The grep saw main.go but not the ignored vendor file; the strict
	// contract was offered as submit_review.
	req := st.reqs[1]
	if out := req.Messages[len(req.Messages)-1].ToolResults[0].Content; !strings.Contains(out, "main.go") || strings.Contains(out, "vendor/") {
		t.Fatalf("grep output = %q", out)
	}
	submit := st.reqs[0].Tools[len(st.reqs[0].Tools)-1]
	if submit.Name != "submit_review" || string(submit.InputSchema) != string(review.SchemaStrict()) {
		t.Fatalf("submit tool = %+v", submit)
	}
	if st.reqs[0].Model != "review" || st.reqs[0].System != "system" || st.reqs[0].MaxTokens != 8192 {
		t.Fatalf("request = %+v", st.reqs[0])
	}
}

func TestReviewAgentTimeout(t *testing.T) {
	head := tree(t, map[string]string{"main.go": "package main\n"})
	st := blockingStepper{}
	logger := slog.New(slog.DiscardHandler)
	res, _ := reviewAgent(t.Context(), st, agentPromptSpec(), head, nil, nil, "s", "u", false, 20*time.Millisecond, logger)
	if res.Stop != agent.StopCanceled || !strings.Contains(res.Err, "timeout") {
		t.Fatalf("result = %+v", res)
	}
}

type blockingStepper struct{}

func (blockingStepper) Step(ctx context.Context, _ model.StepRequest) (model.StepResponse, error) {
	<-ctx.Done()
	return model.StepResponse{}, ctx.Err()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSimilarCode(t *testing.T) {
	chunk := contextpack.Chunk{Stage: contextpack.StageSimilar, Path: "other.go", StartLine: 3, EndLine: 9, Ref: "similarity 0.81", Text: "func a() {}"}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    contextpack.SimilarResponse
		wantErr string
	}{
		{name: "chunks", status: http.StatusOK, body: `{"chunks":[{"stage":"similar","path":"other.go","start_line":3,"end_line":9,` +
			`"ref":"similarity 0.81","text":"func a() {}"}],"indexed":true}`, want: contextpack.SimilarResponse{Chunks: []contextpack.Chunk{chunk}, Indexed: true}},
		{name: "no index", status: http.StatusOK, body: `{"chunks":[],"indexed":false}`, want: contextpack.SimilarResponse{Chunks: []contextpack.Chunk{}}},
		{name: "refused", status: http.StatusTooManyRequests, body: `{"error":{"code":"budget_exceeded"}}` + "\n",
			wantErr: `429 Too Many Requests: {"error":{"code":"budget_exceeded"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path, auth, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				path, auth, body = r.Method+" "+r.URL.Path, r.Header.Get("Authorization"), string(b)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := similarCode(context.Background(), srv.URL+"/", "krk_run", contextpack.SimilarRequest{Queries: []string{"main.go\nfunc b() {}"}, Exclude: []string{"main.go"}})
			if path != "POST /v1/similar" || auth != "Bearer krk_run" || body != `{"queries":["main.go\nfunc b() {}"],"exclude":["main.go"]}` {
				t.Fatalf("request = %s with %q: %s", path, auth, body)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.HasSuffix(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to end %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got.Chunks, tc.want.Chunks) || got.Indexed != tc.want.Indexed {
				t.Fatalf("similarCode = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

// TestHunkQueries: each hunk is led by its path, the removed lines are
// left out, and the count and size are bounded.
func TestHunkQueries(t *testing.T) {
	got := hunkQueries(agentDiff)
	if want := []string{"main.go\npackage main\n\nfunc b() {}\n"}; !slices.Equal(got, want) {
		t.Fatalf("hunkQueries = %q, want %q", got, want)
	}
	var many strings.Builder
	for range contextpack.SimilarQueries + 3 {
		many.WriteString(agentDiff)
	}
	if got := hunkQueries(many.String()); len(got) != contextpack.SimilarQueries {
		t.Fatalf("%d queries, want %d", len(got), contextpack.SimilarQueries)
	}
	if got := hunkQueries(""); len(got) != 0 {
		t.Fatalf("queries of an empty diff = %q", got)
	}
}

// TestSearchTool: search_code asks the gateway once per call, formats what
// it gets back, and stops at its call limit.
func TestSearchTool(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req contextpack.SimilarRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Queries) != 1 || req.Queries[0] != "where is the retry" || !slices.Equal(req.Exclude, []string{"main.go"}) {
			t.Errorf("request = %+v", req)
		}
		_, _ = w.Write([]byte(`{"chunks":[{"stage":"similar","path":"retry.go","start_line":3,"end_line":9,"symbol":"retry","kind":"function",` +
			`"ref":"similarity 0.81","text":"func retry() {}"}],"indexed":true}`))
	}))
	defer srv.Close()
	tool := &searchTool{gatewayURL: srv.URL, token: "krk_run", exclude: []string{"main.go"}, maxBytes: 1 << 10}
	if def := tool.Def(); def.Name != "search_code" {
		t.Fatalf("tool name = %q", def.Name)
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"where is the retry"}`))
	if err != nil || out != "retry.go:3-9 (function retry) similarity 0.81\nfunc retry() {}" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":" "}`)); err == nil {
		t.Fatal("an empty query ran")
	}
	for range searchCalls - 1 {
		if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"where is the retry"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"where is the retry"}`)); err == nil || calls != searchCalls {
		t.Fatalf("past the limit: err=%v calls=%d", err, calls)
	}
}
