package webapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json")

//go:fix inline

var (
	t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(90 * time.Second)
)

var goldenReview = Review{
	ID: "rev-1", Status: store.ReviewCompleted, Trigger: "push", Mode: configfile.ReviewAgentic, Scope: review.ScopeIncremental,
	Model: "acme/large", HeadSHA: "abc123", CostUSD: 0.42, Tokens: TokenCounts{Input: 1000, Output: 200}, DurationMs: new(int64(90000)),
	CreatedAt: t0, FinishedAt: &t1, SkipReason: repoconfig.SkipFiltered, Error: "",
}

var goldenSummary = TenantSummary{
	Slug: "alpha", ManagedBy: configfile.OriginDashboard, Role: auth.RoleAdmin, Installations: 1, Repositories: 3, Reviews7d: 9,
	Usage: MonthUsage{Tokens: 5000, CostUSD: 1.5, TokensPerMonth: 1000000, ReviewsToday: 2, ReviewsPerDay: 50},
}

var goldenRepo = Repository{
	ID: "repo-1", FullName: "alpha/one", Installation: "alpha-bot", Enabled: true, ManagedBy: "file", DefaultBranch: "main",
	Index:      IndexState{ActiveCommit: "def456", ActiveAt: &t0, LastRunStatus: store.IndexCompleted, LastRunAt: &t1},
	LastReview: &ReviewRef{ID: "rev-1", Status: store.ReviewCompleted, CreatedAt: t0},
}

var goldenIndexRun = IndexRun{
	ID: "ix-1", Repository: "alpha/one", CommitSHA: "def456", BaseSHA: "", EmbedModel: "embed", Mode: "full",
	Status: store.IndexCompleted, Trigger: "push", ChunkCount: 12, Error: "", CreatedAt: t0, FinishedAt: &t1,
}

var goldenPull = Pull{
	Repository: "alpha/one", Number: 7, Title: "Add widgets", Author: "ada", State: "open", Draft: false, Merged: false,
	HeadSHA: "abc123", HeadRef: "widgets", BaseRef: "main", URL: "https://git.example/alpha/one/pulls/7", OpenedAt: &t0,
	UpdatedAt: t1, Labels: []Label{{Name: "bug", Color: "ff0000"}},
	LastReview: &ReviewBrief{
		ID: "rev-1", Status: store.ReviewCompleted, Mode: configfile.ReviewSingle, Scope: review.ScopeFull,
		Findings: SeverityCounts{Blocking: 1, Important: 2, Nit: 3}, CreatedAt: t0,
	},
}

var goldenFollowup = Followup{
	ID: "fu-1", CommentID: 99, Repository: "alpha/one", Number: 7, Author: "bob", Inline: true, Path: "a.go", Line: 4,
	Status: store.FollowupAnswered, Reason: "", ReplyCommentID: new(int64(100)), Model: "acme/large", CreatedAt: t0,
}

var goldenTools = []ToolDef{{Name: "grep", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}

// goldens is one populated instance of every DTO, by golden file name.
var goldens = map[string]any{
	"page": Page[Repository]{Items: []Repository{goldenRepo}, NextCursor: new("opaque")},
	"me": Me{
		Account:  Account{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com", AvatarURL: "https://img.example/a.png"},
		Operator: true, Tenants: []TenantMembership{{Slug: "alpha", Role: auth.RoleMember, ManagedBy: configfile.OriginFile}},
	},
	"tenant_summary":  goldenSummary,
	"operator_tenant": OperatorTenant{TenantSummary: goldenSummary, Live: false, Revision: 3},
	"tenant_detail": TenantDetail{
		Slug: "alpha", ManagedBy: configfile.OriginFile, Role: auth.RoleMember,
		Installations: []Installation{{
			Name: "alpha-bot", Forge: configfile.ForgeGitHub, Host: "github.com", Account: "alpha", CredentialKind: CredentialApp,
			Credentials: CredentialsSet{ClientID: true, PrivateKey: true, Token: false, GitToken: false, WebhookSecret: true},
			HookPath:    "/hooks/alpha-bot",
		}},
		Models: Models{Review: "openrouter/acme-large", Fallback: "openrouter/acme-small"},
		Limits: Limits{Concurrency: 2, ReviewsPerDay: 50, TokensPerMonth: 1000000}, Filter: "!pr.draft",
		Usage: goldenSummary.Usage,
	},
	"repository": goldenRepo,
	"repo_detail": RepoDetail{
		Repository: goldenRepo,
		Settings: RepoSettings{
			Enabled: true, Mode: configfile.ReviewAgentic, Models: Models{Review: "openrouter/acme-large"}, Filter: "true", Forks: false,
			Ignore: []string{"vendor/**"}, SettleSeconds: 30, MaxDeltaFiles: 40,
			Review: ReviewBlock{Instructions: []string{"docs/review.md"}, RequireSuggestedFix: true},
			Agent: AgentLimits{
				MaxSteps: 60, MaxToolOutputBytes: 32768, MaxTokens: 4000000, TimeoutSeconds: 1200, Commands: []string{"go"},
				CommandTimeoutSeconds: 30,
			},
			Limits: Limits{Concurrency: 2},
		},
		IndexRuns: []IndexRun{goldenIndexRun},
	},
	"index_run": goldenIndexRun,
	"pull":      goldenPull,
	"review":    goldenReview,
	"followup":  goldenFollowup,
	"pull_detail": PullDetail{
		Pull: goldenPull, Reviews: []Review{goldenReview}, Followups: []Followup{goldenFollowup},
	},
	"review_detail": ReviewDetail{
		Review: ReviewInfo{
			Review: goldenReview, Pull: PullRef{Repository: "alpha/one", Number: 7, Title: "Add widgets"}, ScopeReason: "delta",
			MergeBaseSHA: "base1", PatchID: "patch1", PriorReviewID: new("rev-0"), CancelRequestedAt: nil,
		},
		Summary: &Summary{Take: "Looks fine.", Praise: []string{"tests"}},
		Findings: []Finding{{
			ID: "f-1", Path: "a.go", Line: 3, EndLine: 5, Severity: review.SeverityBlocking, Title: "nil deref",
			Explanation: "x may be nil", SuggestedFix: "check x", Replacement: "if x != nil {}", AgentPrompt: "fix it",
			Fingerprint: "fp", PostedInline: true, ForgeCommentID: new(int64(55)), CreatedAt: t0,
		}},
		RunnerRun: &RunnerRun{
			ID: "run-1", Phase: "done", JobName: "job", PodName: "pod", NodeName: "node", CreatedAt: t0, ScheduledAt: &t0,
			StartedAt: &t0, FinishedAt: &t1, HeartbeatAt: &t1, ExitCode: new(0), TerminationReason: "Completed",
			DeadlineExceeded: false, Error: "", LogTail: "ok\n",
		},
		AgentRun: &AgentRun{
			StopReason: "submitted", Steps: 3, ToolCalls: map[string]int{"grep": 2},
			Timeline: []TimelineStep{{Index: 0, Tools: []string{"grep"}, DurationMs: 1200, OutputBytes: 300, InputTokens: 100, OutputTokens: 20}},
			Sources:  []string{"https://docs.example"}, Usage: Usage{Input: 100, CacheRead: 50, CacheWrite: 10, Output: 20},
			CostUSD: 0.1, Model: "acme/large", Error: "", CreatedAt: t1, Result: json.RawMessage(`{"findings":[]}`),
		},
		Usage: []UsageRow{{Role: "review", Model: "acme/large", Upstream: "acme", InputTokens: 100, OutputTokens: 20, CostUSD: 0.1, CreatedAt: t1}},
		ContextPack: &ContextPack{
			HeadSHA: "abc123", BaseSHA: "base1", PatchID: "patch1", ChangedPaths: []string{"a.go"}, DeltaPaths: []string{"a.go"},
			PriorHeadSHA: new("abc000"),
			Stages: []Stage{{
				Stage: "definitions", Path: "b.go", Language: "go", Symbol: "F", Kind: "func", Scope: "pkg", StartLine: 1,
				EndLine: 9, Ref: "F", Bytes: 120,
			}},
			RepoNotes: []string{"docs/missing.md: not found"}, RepoFiles: []RepoFile{{Path: ".kritik.yaml", Size: 42}}, CreatedAt: t0,
		},
	},
	"review_diff": ReviewDiff{Diff: "diff --git a/a.go b/a.go\n", DeltaDiff: ""},
	"review_raw": ReviewRaw{
		RepoFiles: map[string]string{".kritik.yaml": "mode: agentic\n"},
		Stages: []ContextChunk{{
			Stage: "definitions", Path: "b.go", Language: "go", Symbol: "F", Kind: "func", Scope: "pkg", StartLine: 1, EndLine: 9,
			Ref: "F", Text: "func F() {}",
		}},
		Result: json.RawMessage(`{"findings":[]}`), LogTail: "ok\n",
	},
	"transcript": Transcript{
		System: "You review code.", Tools: goldenTools,
		Turns: []Turn{{
			Index: 0, ID: "mc-1", Kind: transcript.KindAgentStep, Step: 0, Model: "acme/large", Upstream: "acme",
			System: new("You review code, again."), Tools: goldenTools, Reset: false, MessagesFrom: 0,
			Messages: []Message{{
				Role: model.RoleUser, Text: "review this",
				ToolCalls:   []ToolCall{{ID: "c1", Name: "grep", Input: json.RawMessage(`{"q":"x"}`)}},
				ToolResults: []ToolResult{{CallID: "c0", Content: "hit", IsError: false, TruncatedBytes: 10}},
			}},
			Response: Response{Text: "done", ToolCalls: []ToolCall{}, Stop: model.StopEndTurn},
			Usage:    Usage{Input: 10, CacheRead: 5, CacheWrite: 1, Output: 2}, CostUSD: 0.01, DurationMs: 1500, Error: "",
			Truncated: false, CreatedAt: t0, RunnerRunID: "run-1",
		}},
	},
	"usage_series": UsageSeries{
		Group: store.UsageByDay, From: t0, To: t1,
		Rows: []UsagePoint{{Key: "2026-09-01", InputTokens: 100, CacheReadTokens: 50, CacheWriteTokens: 5, OutputTokens: 20, CostUSD: 0.1, Calls: 2}},
	},
	"job": Job{
		ID: 42, Kind: "review", State: store.JobRetryable, Attempt: 2, MaxAttempts: 5, CreatedAt: t0, ScheduledAt: t1,
		AttemptedAt: &t0, FinalizedAt: nil, LastError: "boom",
		Args: JobArgs{Repository: "alpha/one", Number: 7, Head: "abc123", Trigger: "push", CommentID: 0},
	},
	"event": Event{Kind: store.EventReview, Tenant: "alpha", ID: "rev-1", ReviewID: new("rev-1")},
	"error": ErrorBody{Code: CodeNotFound, Message: "tenant not found", Details: json.RawMessage(`{"slug":"x"}`)},
}

func TestDTOGolden(t *testing.T) {
	for name, v := range goldens {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			// Compared compacted: the repository's JSON formatter re-wraps
			// the files, which changes no name, order or value.
			var gotC, wantC bytes.Buffer
			if err := json.Compact(&gotC, got); err != nil {
				t.Fatal(err)
			}
			if err := json.Compact(&wantC, want); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if !bytes.Equal(gotC.Bytes(), wantC.Bytes()) {
				t.Errorf("%s changed; the UI's types.ts mirrors it. got:\n%s", path, got)
			}
		})
	}
}
