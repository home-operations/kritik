package repoconfig

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/tasks"
)

func TestParse_Tasks(t *testing.T) {
	t.Parallel()
	f, _, err := Parse([]byte("tasks:\n  - name: triage\n    on: [{ issue: [opened] }]\n    prompt: .kritik/triage.md\n" +
		"    actions: { comment: { template: .kritik/comment.md } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Referenced(); !slices.Equal(got, []string{".kritik/triage.md", ".kritik/comment.md"}) {
		t.Fatalf("Referenced() = %v", got)
	}
	for name, c := range map[string]struct{ doc, want string }{
		"a bad task":       {"tasks: [{ name: a, on: [] }]\n", "repoconfig: tasks[0]: on needs"},
		"a repeated name":  {"tasks: [{ name: a, on: [{ issue: [] }] }, { name: a, on: [{ comment: [] }] }]\n", "tasks[1]: name \"a\" is used twice"},
		"an escaping path": {"tasks: [{ name: a, on: [{ issue: [] }], prompt: ../x }]\n", "escapes"},
		"an unknown key":   {"tasks: [{ name: a, on: [{ issue: [] }], fields: { x: { type: string, bogus: 1 } } }]\n", "bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := Parse([]byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse() = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestMerge_Tasks(t *testing.T) {
	t.Parallel()
	opTask := tasks.Task{Name: "triage", On: []tasks.Trigger{{Event: tasks.EventIssue}}}
	op := func(b tasks.Bounds) configfile.Settings {
		s := operator()
		s.Tasks, s.TaskBounds = []tasks.Task{opTask}, b
		s.Allow.Models = []configfile.ModelRef{"p/big", "p/small"}
		return s
	}
	on := tasks.Bounds{
		Enabled: true, Events: tasks.DefaultEvents, Actions: tasks.DefaultActions, Context: tasks.DefaultContext,
		RepositoryTasks: true, MaxTasks: 2, MaxFields: tasks.DefaultMaxFields,
	}
	const doc = "tasks:\n" +
		"  - { name: triage, on: [{ issue: [] }] }\n" +
		"  - { name: dupes, on: [{ issue: [opened] }], models: { review: p/huge, fallback: p/small }, agent: { maxSteps: 100, timeout: 5m } }\n" +
		"  - { name: releases, on: [{ raw: { event: release } }] }\n" +
		"  - { name: welcome, on: [{ pull_request: [opened] }], actions: { assign: { propose: { users: [a] } } } }\n" +
		"  - { name: extra, on: [{ comment: [] }] }\n"

	names := func(ts []tasks.Task) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			out[i] = t.Name
		}
		return out
	}
	notes := func(ns []tasks.Note) string {
		out := make([]string, len(ns))
		for i, n := range ns {
			out[i] = n.String()
		}
		return strings.Join(out, "\n")
	}

	m, err := Merge([]byte(doc), op(on))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m.Tasks); !slices.Equal(got, []string{"triage", "dupes", "welcome"}) {
		t.Fatalf("tasks %v\n%s", got, notes(m.TaskNotes))
	}
	want := strings.Join([]string{
		"task triage: task: an operator task has the same name",
		"task releases: on[0] raw:release.*: the event is not allowed",
		"task releases: task: no trigger is left",
		"task welcome: actions.assign: the action is not allowed",
		"task extra: task: over the limit of 2 tasks",
		`task dupes: models.review: "p/huge" was dropped; allowed: p/big, p/small`,
		"task dupes: agent.maxSteps: 100 was dropped; allowed: at most 30",
	}, "\n")
	if got := notes(m.TaskNotes); got != want {
		t.Fatalf("notes:\n%s\nwant:\n%s", got, want)
	}
	dupes := m.Tasks[1]
	if dupes.Models.Review != "" || dupes.Models.Fallback != "p/small" || dupes.Agent.MaxSteps != nil || *dupes.Agent.Timeout != 5*time.Minute {
		t.Fatalf("dupes chose %+v %+v", dupes.Models, dupes.Agent)
	}

	off, err := Merge([]byte(doc), op(tasks.Bounds{}))
	if err != nil || len(off.Tasks) != 0 {
		t.Fatalf("disabled tasks ran: %v, %v", names(off.Tasks), err)
	}
	none, err := Merge(nil, op(on))
	if err != nil || !slices.Equal(names(none.Tasks), []string{"triage"}) {
		t.Fatalf("without a file: %v, %v", names(none.Tasks), err)
	}
	bad, err := Merge([]byte("tasks: [{ name: Bad, on: [{ issue: [] }] }]\n"), op(on))
	if err == nil || !slices.Equal(names(bad.Tasks), []string{"triage"}) {
		t.Fatalf("a file that does not parse: %v, %v", names(bad.Tasks), err)
	}
}
