package tasks

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPlan(t *testing.T) {
	t.Parallel()
	triaged := prepared(t, triage, triageFiles())
	repoLabels := []string{"bug", "enhancement", "area/api", "area/runner", "needs-triage", "needs-info", "priority/p1", "priority/p0"}
	allowed := func(login string) bool { return login != "carol" }
	issue := SampleInput()
	pull := SampleInput()
	pull.Event, pull.Subject.Kind, pull.Subject.Author = EventPullRequest, SubjectPull, "dave"
	answer := func(edit func(*Answer)) Answer {
		a := Answer{
			Summary: "Looks like an API bug.",
			Fields:  map[string]any{"priority": "p1", "area": "api", "needsInfo": false, "missing": []any{}},
			Labels:  LabelChanges{Add: []string{"bug"}, Remove: []string{"needs-triage"}},
		}
		if edit != nil {
			edit(&a)
		}
		return a
	}
	tests := []struct {
		name    string
		p       *Prepared
		in      Input
		a       Answer
		want    Plan
		dropped []Drop
	}{
		{
			name: "proposals and rules together", p: triaged, in: issue, a: answer(nil),
			want: Plan{AddLabels: []string{"bug", "priority/p1", "area/api"}, RemoveLabels: []string{"needs-triage"}},
		},
		{
			name: "a rule's if", p: triaged, in: issue, a: answer(func(a *Answer) { a.Fields["needsInfo"] = true }),
			want: Plan{AddLabels: []string{"bug", "priority/p1", "area/api", "needs-info"}, RemoveLabels: []string{"needs-triage"}},
		},
		{
			name: "labels the repository lacks or the propose globs exclude", p: triaged, in: issue,
			a: answer(func(a *Answer) {
				a.Labels.Add = []string{"bug", "area/web", "wontfix", "bug"}
				a.Fields["priority"] = "p3"
			}),
			want: Plan{AddLabels: []string{"bug", "area/api"}, RemoveLabels: []string{"needs-triage"}},
			dropped: []Drop{
				{dropAddLabel, "wontfix", "it is not a label the task lets the model add"},
				{dropAddLabel, "area/web", "the repository has no such label"},
				{dropAddLabel, "priority/p3", "the repository has no such label"},
			},
		},
		{
			name: "an add wins over a remove", p: prepared(t, "name: a\non: [{issue: []}]\nactions:\n  labels:\n"+
				"    propose: {add: [bug], remove: [bug]}\n", nil), in: issue,
			a:       Answer{Labels: LabelChanges{Add: []string{"bug"}, Remove: []string{"bug"}}},
			want:    Plan{AddLabels: []string{"bug"}},
			dropped: []Drop{{dropRemoveLabel, "bug", "it is also added, and an add wins"}},
		},
		{
			name: "a state change the task declares", p: triaged, in: issue, a: answer(func(a *Answer) { a.State = StateClose }),
			want: Plan{AddLabels: []string{"bug", "priority/p1", "area/api"}, RemoveLabels: []string{"needs-triage"}, State: StateClose},
		},
		{
			name: "a state change the task does not declare", p: triaged, in: issue, a: answer(func(a *Answer) { a.State = StateReopen }),
			want:    Plan{AddLabels: []string{"bug", "priority/p1", "area/api"}, RemoveLabels: []string{"needs-triage"}},
			dropped: []Drop{{ActionState, StateReopen, "the task does not let the model reopen the subject"}},
		},
		{
			name: "a false block if drops the proposal", p: triaged, in: pull, a: answer(func(a *Answer) { a.State = StateClose }),
			want:    Plan{AddLabels: []string{"bug", "priority/p1", "area/api"}, RemoveLabels: []string{"needs-triage"}},
			dropped: []Drop{{ActionState, StateClose, "its if is false"}},
		},
		{
			name: "users outside the allowlist or not allowed", p: prepared(t, "name: a\non: [{issue: []}]\nactions:\n"+
				"  assign: {propose: {users: [alice, carol]}, rules: [{users: ['{{ .Answer.Summary }}']}]}\n", nil), in: issue,
			a:    Answer{Summary: "not a login", Assignees: []string{"alice", "mallory", "carol"}},
			want: Plan{Assignees: []string{"alice"}},
			dropped: []Drop{
				{ActionAssign, "mallory", "it is not a user the task lets the model pick"},
				{ActionAssign, "carol", "the user is not allowed"},
				{ActionAssign, "not a login", "it is not a valid login"},
			},
		},
		{
			name: "reviewers from a rule, never the author", p: prepared(t, "name: a\non: [{pull_request: []}]\nactions:\n"+
				"  reviewers: {rules: [{users: [bob, dave], if: 'answer.fields.area == \"runner\"'}]}\nfields: {area: {type: string}}\n", nil),
			in: pull, a: Answer{Fields: map[string]any{"area": "runner"}},
			want:    Plan{Reviewers: []string{"bob"}},
			dropped: []Drop{{ActionReviewers, "dave", "the author cannot review their own pull request"}},
		},
		{
			name: "reviewers on an issue", p: prepared(t, "name: a\non: [{issue: []}]\nactions: {reviewers: {propose: {users: [bob]}}}\n", nil),
			in: issue, a: Answer{Reviewers: []string{"bob"}},
			dropped: []Drop{{ActionReviewers, "bob", "reviews can only be requested on a pull request"}},
		},
		{
			name: "inline comments on an issue", p: triaged, in: issue,
			a:       answer(func(a *Answer) { a.Inline = []Inline{{Path: "a.go", Line: 1, Body: "x"}} }),
			want:    Plan{AddLabels: []string{"bug", "priority/p1", "area/api"}, RemoveLabels: []string{"needs-triage"}},
			dropped: []Drop{{dropInline, "a.go:1", "inline comments need a pull request"}},
		},
		{
			name: "inline comments on a pull request, deduplicated", p: prepared(t, "name: a\non: [{pull_request: []}]\n"+
				"actions: {inlineComments: {propose: true}}\n", nil), in: pull,
			a:    Answer{Inline: []Inline{{Path: "a.go", Line: 1, Body: "x"}, {Path: "a.go", Line: 1, Body: "x"}}},
			want: Plan{Inline: []Inline{{Path: "a.go", Line: 1, Body: "x"}}},
		},
		{
			name: "actions the task does not declare", p: prepared(t, "name: a\non: [{issue: []}]\n", nil), in: issue,
			a: Answer{Labels: LabelChanges{Add: []string{"bug"}}, State: StateClose, Assignees: []string{"alice"}},
			dropped: []Drop{
				{dropAddLabel, "bug", "the task declares no labels action"},
				{ActionState, StateClose, "the task declares no state action"},
				{ActionAssign, "alice", "the task declares no assign action"},
			},
		},
		{
			name: "a rule whose if fails", p: prepared(t, "name: a\non: [{issue: []}]\nactions:\n"+
				"  labels: {rules: [{add: [bug], if: 'answer.fields.nope'}]}\n", nil), in: issue, a: Answer{},
			dropped: []Drop{{ActionLabels, "answer.fields.nope", "its if failed: tasks: eval \"answer.fields.nope\": no such key: nope"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.p.Plan(tt.in, tt.a, Facts{RepoLabels: repoLabels, UserAllowed: allowed})
			if err != nil {
				t.Fatal(err)
			}
			got.Comment = nil
			tt.want.Dropped = tt.dropped
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Plan() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestPlan_Comment(t *testing.T) {
	t.Parallel()
	p := prepared(t, triage, triageFiles())
	facts := Facts{RepoLabels: []string{"bug", "priority/p1"}, UserAllowed: func(string) bool { return true }}
	a := Answer{
		Summary: "Summary <!-- kritik:task:other --> here", Fields: map[string]any{"priority": "p1", "area": "api", "needsInfo": false},
		Labels: LabelChanges{Add: []string{"bug"}},
	}
	got, err := p.Plan(SampleInput(), a, facts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Comment == nil || got.Comment.Mode != CommentSticky {
		t.Fatalf("comment = %+v", got.Comment)
	}
	if strings.Contains(got.Comment.Body, "<!-- kritik") || !strings.Contains(got.Comment.Body, "labels bug, priority/p1") {
		t.Fatalf("comment body %q", got.Comment.Body)
	}

	a.Labels.Add = nil
	facts.RepoLabels = nil
	got, _ = p.Plan(SampleInput(), a, facts)
	if got.Comment != nil || !slices.Contains(got.Dropped, Drop{dropComment, CommentSticky, "its if is false"}) {
		t.Fatalf("comment %+v posted though its if is false; dropped %+v", got.Comment, got.Dropped)
	}

	def := prepared(t, "name: a\non: [{issue: []}]\nactions: {comment: {}, labels: {propose: {add: [bug]}}}\n", nil)
	got, _ = def.Plan(SampleInput(), Answer{Summary: "Done.", Labels: LabelChanges{Add: []string{"bug"}}}, Facts{RepoLabels: []string{"bug"}})
	if got.Comment == nil || got.Comment.Body != "Done.\n\n- Added labels: bug" {
		t.Fatalf("default comment %+v", got.Comment)
	}

	release := prepared(t, "name: a\non: [{raw: {event: release}}]\nactions: {comment: {mode: none}}\n", nil)
	if got, _ := release.Plan(Input{RawEvent: "release"}, Answer{Summary: "x"}, Facts{}); got.Comment != nil {
		t.Fatalf("mode none posted %+v", got.Comment)
	}
}
