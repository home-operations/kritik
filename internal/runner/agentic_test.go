package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/agent"
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
	s := agenticSpec()
	s.PriorHead = shaB
	return s
}

func TestAgentPrompt(t *testing.T) {
	pack := packView{
		Diff: agentDiff, Changed: []string{"main.go"},
		Context: []contextpack.Chunk{{Stage: contextpack.StageDefinition, Path: "util.go", StartLine: 1, EndLine: 2, Text: "func u() {}"}},
		Scope:   review.ScopeFull,
	}
	operator := []string{"docs/rules.md"}
	tests := []struct {
		name         string
		files        repoconfig.Files
		scope        review.Scope
		instructions []string
		strict       bool
	}{
		{name: "operator instructions and strictness", files: repoconfig.Files{"docs/rules.md": "Operator rules."},
			scope: review.ScopeFull, instructions: []string{"Operator rules."}, strict: true},
		{
			name: "the merge-base file's instructions and strictness replace the operator's",
			files: repoconfig.Files{
				repoconfig.FileName: "review:\n  instructions: [.kritik/rules.md]\n  requireSuggestedFix: false\n",
				".kritik/rules.md":  "Repository rules.", "docs/rules.md": "Operator rules.",
			},
			scope: review.ScopeFull, instructions: []string{"Repository rules."},
		},
		{
			name:  "a file that does not parse leaves the operator's settings",
			files: repoconfig.Files{repoconfig.FileName: "unknown: 1\n", "docs/rules.md": "Operator rules."},
			scope: review.ScopeFull, instructions: []string{"Operator rules."}, strict: true,
		},
		{name: "incremental adds the delta and the prior findings", files: repoconfig.Files{}, scope: review.ScopeIncremental, strict: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentPromptSpec()
			s.Prompt.Instructions, s.Prompt.RequireSuggestedFix = operator, true
			pack := pack
			pack.Scope = tt.scope
			if tt.scope == review.ScopeIncremental {
				pack.DeltaDiff = agentDiff
			}
			system, user, strict := agentPrompt(s, tt.files, pack)
			if want := review.SystemPrompt(tt.instructions); system != want {
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

func TestAgentSkip(t *testing.T) {
	tests := []struct {
		name    string
		files   repoconfig.Files
		changed []string
		patchID string
		want    string
	}{
		{name: "reviewed", files: repoconfig.Files{}, changed: []string{"main.go"}, patchID: "p2"},
		{name: "unchanged bot patch", files: repoconfig.Files{}, changed: []string{"main.go"}, patchID: "p1", want: "unchanged patch"},
		{name: "disabled", files: repoconfig.Files{repoconfig.FileName: "enabled: false\n"}, changed: []string{"main.go"}, patchID: "p2",
			want: "disabled"},
		{
			name: "only skipped paths", files: repoconfig.Files{repoconfig.FileName: "skip:\n  onlyPaths: ['**/*.md']\n"},
			changed: []string{"docs/a.md"}, patchID: "p2", want: "only skipped paths",
		},
		{name: "a broken file skips nothing", files: repoconfig.Files{repoconfig.FileName: "enabled: [\n"}, changed: []string{"main.go"},
			patchID: "p2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentPromptSpec()
			s.Prompt.UnchangedPatchID = "p1"
			if got := agentSkip(s, tt.files, tt.changed, tt.patchID); got != tt.want {
				t.Fatalf("agentSkip = %q, want %q", got, tt.want)
			}
		})
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	res, timeline := reviewAgent(t.Context(), st, s, head, []string{"vendor/**"}, "system", "user", true, time.Minute, logger)
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
	if st.reqs[0].Model != "example-model" || st.reqs[0].System != "system" || st.reqs[0].MaxTokens != 8192 {
		t.Fatalf("request = %+v", st.reqs[0])
	}
}

func TestReviewAgentTimeout(t *testing.T) {
	head := tree(t, map[string]string{"main.go": "package main\n"})
	st := blockingStepper{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	res, _ := reviewAgent(t.Context(), st, agentPromptSpec(), head, nil, "s", "u", false, 20*time.Millisecond, logger)
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
