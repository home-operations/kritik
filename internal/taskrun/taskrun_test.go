package taskrun

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/tasks"
	"github.com/home-operations/kritik/internal/webhook"
)

func TestInput(t *testing.T) {
	repo := &webhook.Repository{FullName: "acme/widgets", DefaultBranch: "main"}
	raw := json.RawMessage(`{"action":"opened","number":7}`)
	tests := []struct {
		name string
		ev   webhook.Event
		want tasks.Input
	}{
		{"issue", webhook.Event{
			Kind: webhook.KindIssue, Action: "opened", Forge: configfile.ForgeGitea, RawEvent: "issues", Sender: "devin", Raw: raw,
			Repository: repo, Subject: &webhook.Subject{Kind: webhook.SubjectIssue, Number: 7},
			Issue: &webhook.Issue{Number: 7, Title: "t", Body: "b", State: "open", Author: "devin", Labels: []string{"bug"}, URL: "u"},
		}, tasks.Input{
			Forge: "gitea", Event: tasks.EventIssue, RawEvent: "issues", Action: "opened", Sender: "devin",
			Subject: &tasks.Subject{Kind: "issue", Number: 7, Title: "t", Body: "b", State: "open", Author: "devin", URL: "u", Labels: []string{"bug"}},
			Raw:     map[string]any{"action": "opened", "number": float64(7)},
			Repo:    tasks.Repo{Owner: "acme", Name: "widgets", DefaultBranch: "main"},
		}},
		{"pull request", webhook.Event{
			Kind: webhook.KindPullRequest, Action: "labeled", Forge: configfile.ForgeGitHub, RawEvent: "pull_request", Repository: repo,
			PullRequest: &webhook.PullRequest{Number: 3, Title: "p", State: "open", Draft: true, Labels: []webhook.Label{{Name: "x"}}},
		}, tasks.Input{
			Forge: "github", Event: tasks.EventPullRequest, RawEvent: "pull_request", Action: "labeled",
			Subject: &tasks.Subject{Kind: "pull", Number: 3, Title: "p", State: "open", Draft: true, Labels: []string{"x"}},
			Repo:    tasks.Repo{Owner: "acme", Name: "widgets", DefaultBranch: "main"},
		}},
		{"comment", webhook.Event{
			Kind: webhook.KindComment, Action: "created", RawEvent: "issue_comment", Repository: repo,
			Subject: &webhook.Subject{Kind: webhook.SubjectIssue, Number: 9}, Comment: &webhook.Comment{ID: 1, Number: 9},
		}, tasks.Input{
			Event: tasks.EventComment, RawEvent: "issue_comment", Action: "created", Subject: &tasks.Subject{Kind: "issue", Number: 9},
			Repo: tasks.Repo{Owner: "acme", Name: "widgets", DefaultBranch: "main"},
		}},
		{"ignored", webhook.Event{Kind: webhook.KindIgnored, Action: "published", RawEvent: "release", Raw: json.RawMessage(`[1]`)},
			tasks.Input{RawEvent: "release", Action: "published"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Input(tt.ev); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Input = %+v\nwant   %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeRaw(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		nil  bool
	}{
		{"object", `{"a":1}`, false},
		{"empty", ``, true},
		{"not an object", `"x"`, true},
		{"not json", `{`, true},
		{"too large", `{"a":"` + strings.Repeat("x", MaxRawBytes) + `"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecodeRaw([]byte(tt.raw)); (got == nil) != tt.nil {
				t.Fatalf("DecodeRaw = %v, want nil %v", got, tt.nil)
			}
		})
	}
}

func TestSubject(t *testing.T) {
	got := Subject(forge.Issue{Number: 4, Title: "t", State: "closed", Author: "a", Labels: []string{"l"}, Assignees: []string{"b"}, IsPull: true, URL: "u"}, nil)
	want := &tasks.Subject{Kind: "pull", Number: 4, Title: "t", State: "closed", Author: "a", URL: "u", Labels: []string{"l"}, Assignees: []string{"b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Subject = %+v, want %+v", got, want)
	}
	if Subject(forge.Issue{Number: 1}, nil).Kind != tasks.SubjectIssue {
		t.Fatal("an issue is not a pull request")
	}

	raw := func(number int, draft bool) map[string]any {
		return map[string]any{"pull_request": map[string]any{"number": float64(number), "draft": draft}}
	}
	drafts := []struct {
		name  string
		issue forge.Issue
		raw   map[string]any
		want  bool
	}{
		{"forge says draft", forge.Issue{Number: 4, IsPull: true, Draft: true}, nil, true},
		{"delivery says draft", forge.Issue{Number: 4, IsPull: true}, raw(4, true), true},
		{"forge draft not clobbered", forge.Issue{Number: 4, IsPull: true, Draft: true}, raw(4, false), true},
		{"another pull request's draft", forge.Issue{Number: 4, IsPull: true}, raw(5, true), false},
		{"ready", forge.Issue{Number: 4, IsPull: true}, raw(4, false), false},
		{"issue", forge.Issue{Number: 4, Draft: true}, raw(4, true), false},
	}
	for _, tt := range drafts {
		t.Run(tt.name, func(t *testing.T) {
			if got := Subject(tt.issue, tt.raw).Draft; got != tt.want {
				t.Fatalf("Draft = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustTasks(t *testing.T, doc string) []tasks.Task {
	t.Helper()
	var ts []tasks.Task
	if err := yaml.Unmarshal([]byte(doc), &ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestCandidate(t *testing.T) {
	operator := mustTasks(t, `
- {name: triage, on: [{issue: [opened]}]}
- {name: releases, on: [{raw: {event: release, actions: [published]}}]}
`)
	on := tasks.Bounds{Enabled: true, Events: tasks.DefaultEvents, RepositoryTasks: true}
	noRepo := on
	noRepo.RepositoryTasks = false
	tests := []struct {
		name     string
		in       tasks.Input
		operator []tasks.Task
		b        tasks.Bounds
		want     bool
	}{
		{"tasks disabled", tasks.Input{Event: "issue", RawEvent: "issues", Action: "opened"}, operator, tasks.Bounds{}, false},
		{"operator trigger", tasks.Input{Event: "issue", RawEvent: "issues", Action: "opened"}, operator, noRepo, true},
		{"operator raw trigger", tasks.Input{RawEvent: "release", Action: "published"}, operator, noRepo, true},
		{"no operator trigger, no repository tasks", tasks.Input{Event: "issue", RawEvent: "issues", Action: "closed"}, operator, noRepo, false},
		{"a repository task may trigger", tasks.Input{Event: "issue", RawEvent: "issues", Action: "closed"}, nil, on, true},
		{"a raw event outside the bounds", tasks.Input{RawEvent: "push"}, nil, on, false},
		{"a raw event the bounds list", tasks.Input{RawEvent: "push"}, nil,
			tasks.Bounds{Enabled: true, Events: []string{"raw:push.*"}, RepositoryTasks: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Candidate(tt.in, tt.operator, tt.b); got != tt.want {
				t.Fatalf("Candidate = %v, want %v", got, tt.want)
			}
		})
	}
}
