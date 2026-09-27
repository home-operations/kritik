package worker

import (
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/store"
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
