package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/taskrun"
	"github.com/home-operations/kritik/internal/tasks"
)

func taskDefs(t *testing.T, doc string) []tasks.Task {
	t.Helper()
	var ts []tasks.Task
	if err := yaml.Unmarshal([]byte(doc), &ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestIsBot(t *testing.T) {
	tests := []struct {
		sender, bot string
		want        bool
	}{
		{"kritik[bot]", "kritik[bot]", true},
		{"Kritik[bot]", "kritik[bot]", true},
		{"devin", "kritik[bot]", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.sender, func(t *testing.T) {
			if got := isBot(tt.sender, tt.bot); got != tt.want {
				t.Fatalf("isBot(%q, %q) = %v", tt.sender, tt.bot, got)
			}
		})
	}
}

func TestMatchTasksAndJobs(t *testing.T) {
	ts := taskDefs(t, `
- {name: triage, mode: single, on: [{issue: [opened]}], if: '!("triaged" in subject.labels)'}
- {name: closer, on: [{issue: [closed]}]}
- {name: broken, on: [{issue: []}], if: 'subject.nope == 1'}
- {name: releases, on: [{raw: {event: release}}]}
`)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	issue := func(action string, labels ...string) tasks.Input {
		return tasks.Input{Event: tasks.EventIssue, RawEvent: "issues", Action: action, Subject: &tasks.Subject{Kind: "issue", Number: 7, Labels: labels}}
	}
	tests := []struct {
		name string
		in   tasks.Input
		want []string
	}{
		{"opened", issue("opened"), []string{"triage"}},
		{"already triaged", issue("opened", "triaged"), nil},
		{"closed", issue("closed"), []string{"closer"}},
		{"raw", tasks.Input{RawEvent: "release", Action: "published"}, []string{"releases"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, m := range matchTasks(ts, tt.in, logger) {
				got = append(got, m.Name)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("matched = %q, want %q", got, tt.want)
			}
		})
	}

	args := jobs.TaskDispatchArgs{TenantID: "t", RepositoryID: "r", EventID: "e"}
	ev := store.TaskEvent{SubjectKind: "issue", SubjectNumber: 7}
	runs, queued := taskJobs(ts[:2], args, ev, []string{"issue.opened", "raw:issues.opened"}, "sha")
	wantRuns := []store.TaskRun{
		{TenantID: "t", RepositoryID: "r", Task: "triage", EventID: "e", SubjectKind: "issue", SubjectNumber: 7, Trigger: "issue.opened",
			Mode: "single", ConfigSHA: "sha"},
		{TenantID: "t", RepositoryID: "r", Task: "closer", EventID: "e", SubjectKind: "issue", SubjectNumber: 7, Trigger: "issue.opened",
			Mode: "agentic", ConfigSHA: "sha"},
	}
	wantJobs := []jobs.TaskArgs{
		{TenantID: "t", RepositoryID: "r", EventID: "e", Task: "triage", ConfigSHA: "sha"},
		{TenantID: "t", RepositoryID: "r", EventID: "e", Task: "closer", ConfigSHA: "sha"},
	}
	if !reflect.DeepEqual(runs, wantRuns) || !reflect.DeepEqual(queued, wantJobs) {
		t.Fatalf("taskJobs = %+v, %+v", runs, queued)
	}
}

func TestTaskPrompt(t *testing.T) {
	ts := taskDefs(t, `
- name: tools
  on: [{issue: []}]
  agent: {tools: [grep, run], commands: [rg]}
  context:
    files: [{path: README.md}, {glob: "docs/*.md", max: 3}]
    commands: [{name: owners, run: "cat  .github/CODEOWNERS"}, {name: search, run: "rg -n TODO"}]
- {name: defaults, on: [{issue: []}]}
- {name: commands-without-run, on: [{issue: []}], agent: {tools: [grep], commands: [rg]}}
`)
	bounds := configfile.Settings{TaskBounds: tasks.Bounds{Tools: []string{"read_file", "list_files", "shell"}}}
	tests := []struct {
		task         *tasks.Task
		want         runner.TaskPrompt
		wantCommands []string
	}{
		{&ts[0], runner.TaskPrompt{
			Name: "tools", System: "sys", User: "user", Schema: []byte(`{}`), Tools: []string{"grep"}, Run: []string{"rg"},
			Files: []runner.TaskFiles{{Glob: "docs/*.md", Max: 3}},
			Commands: []runner.TaskCommand{
				{Name: "owners", Argv: []string{"cat", ".github/CODEOWNERS"}}, {Name: "search", Argv: []string{"rg", "-n", "TODO"}},
			},
			SourceBytes: taskSourceBytes, ContextBytes: 100,
		}, []string{"cat", "rg"}},
		{&ts[1], runner.TaskPrompt{
			Name: "defaults", System: "sys", User: "user", Schema: []byte(`{}`), Tools: []string{"read_file", "list_files"},
			SourceBytes: taskSourceBytes, ContextBytes: 100,
		}, nil},
		{&ts[2], runner.TaskPrompt{
			Name: "commands-without-run", System: "sys", User: "user", Schema: []byte(`{}`), Tools: []string{"grep"},
			SourceBytes: taskSourceBytes, ContextBytes: 100,
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.task.Name, func(t *testing.T) {
			r := &taskRunner{task: tt.task, settings: bounds, contextLeft: 100}
			got := r.taskPrompt("sys", "user", []byte(`{}`))
			if !reflect.DeepEqual(*got, tt.want) {
				t.Fatalf("taskPrompt = %+v\nwant %+v", *got, tt.want)
			}
			if cs := taskCommands(got); !reflect.DeepEqual(cs, tt.wantCommands) {
				t.Fatalf("taskCommands = %q, want %q", cs, tt.wantCommands)
			}
		})
	}
}

func TestRateLimitReason(t *testing.T) {
	tests := []struct {
		name     string
		n, limit int
		limited  bool
	}{
		{"under", 5, 6, false},
		{"at", 6, 6, true},
		{"over", 9, 6, true},
		{"no limit", 100, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rateLimitReason(tt.n, tt.limit); (got != "") != tt.limited {
				t.Fatalf("rateLimitReason(%d, %d) = %q", tt.n, tt.limit, got)
			}
		})
	}
}

func TestThreadTail(t *testing.T) {
	comments := make([]forge.Comment, 0, 25)
	for i := range 25 {
		comments = append(comments, forge.Comment{Author: "a", Body: strings.Repeat("x", i), CreatedAt: time.Unix(int64(i), 0)})
	}
	comments[24].Body = strings.Repeat("é", taskCommentBytes)
	tests := []struct {
		name  string
		n     int
		count int
		first int64
	}{
		{"default", 0, taskThreadComments, 5},
		{"fewer", 3, 3, 22},
		{"more than there are", 50, 25, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := threadTail(comments, tt.n)
			if len(got) != tt.count || got[0].CreatedAt.Unix() != tt.first {
				t.Fatalf("threadTail = %d comments from %d", len(got), got[0].CreatedAt.Unix())
			}
			if last := got[len(got)-1].Body; len(last) > taskCommentBytes || !strings.HasPrefix(strings.Repeat("é", 10), last[:20]) {
				t.Fatalf("last comment is %d bytes", len(last))
			}
		})
	}
}

func TestRelatedIssues(t *testing.T) {
	got := relatedIssues([]forge.Issue{{Number: 7, Title: "self"}, {Number: 3, Title: "other", State: "open", IsPull: true}}, 7)
	if want := []relatedIssue{{Number: 3, Title: "other", State: "open", Pull: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relatedIssues = %+v", got)
	}
}

func TestAttemptEnds(t *testing.T) {
	boom := errors.New("forge down")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	remote, cancelRemote := context.WithCancelCause(context.Background())
	cancelRemote(river.ErrJobCancelledRemotely)
	tests := []struct {
		name              string
		ctx               context.Context
		err               error
		attempt, attempts int
		ends              bool
	}{
		{"an error with attempts left is retried", context.Background(), boom, 1, 5, false},
		{"the last attempt ends the run", context.Background(), boom, 5, 5, true},
		{"a job that cancels itself ends the run", context.Background(), river.JobCancel(boom), 1, 5, true},
		{"a shutdown or timeout is retried", canceled, context.Canceled, 1, 5, false},
		{"a cancel from outside ends the run", remote, context.Canceled, 1, 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attemptEnds(tt.ctx, tt.err, tt.attempt, tt.attempts); got != tt.ends {
				t.Fatalf("attemptEnds = %v, want %v", got, tt.ends)
			}
		})
	}
}

func TestSpend(t *testing.T) {
	r := &taskRunner{}
	r.spend(model.StepRequest{Model: "primary"}, model.StepResponse{Usage: model.Usage{Input: 40, Output: 2}, CostUSD: 0.01})
	r.spend(model.StepRequest{Model: "primary"}, model.StepResponse{})
	r.spend(model.StepRequest{Model: "fallback"}, model.StepResponse{Model: "fallback-1", Upstream: "up", Usage: model.Usage{Input: 30, Output: 10}})
	want := []store.TaskUsage{
		{Model: "primary", Input: 40, Output: 2, CostUSD: 0.01},
		{Model: "fallback-1", Upstream: "up", Input: 30, Output: 10},
	}
	if !reflect.DeepEqual(r.spent, want) {
		t.Fatalf("spent = %+v, want %+v", r.spent, want)
	}
}

// TestDraftGuard checks the pr-area-labels recipe's draft guard against the
// subject dispatch builds from the forge.
func TestDraftGuard(t *testing.T) {
	doc, err := os.ReadFile("../../docs/recipes/pr-area-labels/.kritik.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := repoconfig.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name  string
		issue forge.Issue
		raw   map[string]any
		want  int
	}{
		{"ready", forge.Issue{Number: 2, IsPull: true, State: "open"}, nil, 1},
		{"draft on the forge", forge.Issue{Number: 2, IsPull: true, State: "open", Draft: true}, nil, 0},
		{"draft in the delivery", forge.Issue{Number: 2, IsPull: true, State: "open"},
			map[string]any{"pull_request": map[string]any{"number": float64(2), "draft": true}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tasks.Input{Event: tasks.EventPullRequest, RawEvent: "pull_request", Action: "opened", Raw: tt.raw}
			in.Subject = taskrun.Subject(tt.issue, in.Raw)
			if got := matchTasks(f.Tasks, in, logger); len(got) != tt.want {
				t.Fatalf("matched %d tasks, want %d", len(got), tt.want)
			}
		})
	}
}

// readTokenForge serves ReadGitToken alone.
type readTokenForge struct {
	forge.Client
	err error
}

func (f readTokenForge) ReadGitToken(context.Context, string, string) (string, error) {
	return "", f.err
}

type noExecutor struct{}

func (noExecutor) Run(context.Context, executor.Spec) executor.Result { return executor.Result{} }

func TestAgenticSlotNeedsReadToken(t *testing.T) {
	boom := errors.New("github: mint read-only token: 500")
	tests := []struct {
		name    string
		err     error
		want    store.TaskRunResult
		wantErr error
	}{
		{"no read-only token skips", fmt.Errorf("wrapped: %w", forge.ErrNoReadToken), skipped(noReadToken), nil},
		{"a failed mint is retried", boom, store.TaskRunResult{}, boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &taskRunner{
				w:      &Task{GatewayURL: "http://gateway", Executor: noExecutor{}},
				file:   &configfile.File{Providers: map[string]configfile.Provider{"openrouter": {}}},
				task:   &tasks.Task{Models: tasks.Models{Review: "openrouter/m"}},
				client: readTokenForge{err: tt.err}, owner: "acme", name: "widgets",
			}
			res, release, err := r.agenticSlot(t.Context())
			if release != nil || !errors.Is(err, tt.wantErr) || !reflect.DeepEqual(res, tt.want) {
				t.Fatalf("agenticSlot = %+v, release %v, %v; want %+v, %v", res, release != nil, err, tt.want, tt.wantErr)
			}
		})
	}
}
