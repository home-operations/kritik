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
		s.Allow.Modes = []configfile.ReviewMode{configfile.ReviewSingle, configfile.ReviewAgentic}
		return s
	}
	on := tasks.Bounds{
		Enabled: true, Events: tasks.DefaultEvents, Actions: tasks.DefaultActions, Context: tasks.DefaultContext,
		Tools: tasks.DefaultTools, RepositoryTasks: true, MaxTasks: 2, MaxFields: tasks.DefaultMaxFields,
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

// TestMerge_BoundedTasks checks the tasks a tenant admin writes on the
// dashboard, and the file's, are held to the task bounds and to the
// operator's mode, model, agent and command bounds.
func TestMerge_BoundedTasks(t *testing.T) {
	t.Parallel()
	decode := func(doc string) []tasks.Task {
		f, _, err := Parse([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return f.Tasks
	}
	op := operator()
	op.Tasks = decode("tasks: [{ name: trusted, on: [{ raw: { event: release } }], models: { review: p/huge }, " +
		"context: { commands: [{ name: x, run: curl example.com }] } }]\n")
	op.DashboardTasks = decode("tasks:\n" +
		"  - { name: admin, mode: single, on: [{ issue: [] }, { raw: { event: issues } }], models: { review: p/huge }, system: s.md,\n" +
		"      actions: { state: { propose: { close: true } }, labels: { propose: { add: [bug] } } } }\n" +
		"  - { name: costly, on: [{ issue: [] }] }\n")
	op.TaskBounds = tasks.Bounds{
		Enabled: true, Events: tasks.DefaultEvents, Actions: tasks.DefaultActions, Tools: tasks.DefaultTools,
		Context: append(slices.Clone(tasks.DefaultContext), tasks.ContextCommands), MaxTasks: 5, MaxFields: 5, RepositoryTasks: true,
	}
	const doc = "tasks:\n" +
		"  - { name: admin, mode: single, on: [{ issue: [] }] }\n" +
		"  - { name: owners, mode: agentic, on: [{ issue: [] }] }\n" +
		"  - { name: lookup, mode: single, on: [{ issue: [] }] }\n"
	op.Allow.Modes = nil
	m, err := Merge([]byte(doc), op)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(m.Tasks))
	for _, t := range m.Tasks {
		got = append(got, t.Name)
	}
	if !slices.Equal(got, []string{"trusted", "admin", "lookup"}) || m.DashboardTasks != nil {
		t.Fatalf("tasks %v, dashboard %v", got, m.DashboardTasks)
	}
	trusted, admin := m.Tasks[0], m.Tasks[1]
	if trusted.Models.Review != "p/huge" || len(trusted.Context.Commands) != 1 || len(trusted.On) != 1 {
		t.Fatalf("the file's task was clipped: %+v", trusted)
	}
	if admin.Models.Review != "" || admin.System != "" || admin.Actions.State != nil || admin.Actions.Labels == nil || len(admin.On) != 1 {
		t.Fatalf("the dashboard's task was not clipped: %+v", admin)
	}
	want := strings.Join([]string{
		"task admin: on[1] raw:issues.*: the event is not allowed",
		"task admin: actions.state: the action is not allowed",
		"task admin: system: the operator does not allow a task to add to the system prompt",
		`task admin: models.review: "p/huge" was dropped; allowed: p/big`,
		`task costly: mode: "agentic" was dropped; allowed: single`,
		"task admin: task: an operator task has the same name",
		`task owners: mode: "agentic" was dropped; allowed: single`,
	}, "\n")
	notes := make([]string, 0, len(m.TaskNotes))
	for _, n := range m.TaskNotes {
		notes = append(notes, n.String())
	}
	if strings.Join(notes, "\n") != want {
		t.Fatalf("notes:\n%s\nwant:\n%s", strings.Join(notes, "\n"), want)
	}
}

func TestMerge_ContextCommands(t *testing.T) {
	t.Parallel()
	op := operator()
	op.Allow.Modes = []configfile.ReviewMode{configfile.ReviewAgentic}
	op.TaskBounds = tasks.Bounds{
		Enabled: true, Events: tasks.DefaultEvents, Actions: tasks.DefaultActions, Tools: tasks.DefaultTools, RepositoryTasks: true,
		Context: []string{tasks.ContextCommands}, MaxTasks: 5, MaxFields: 5,
	}
	m, err := Merge([]byte("tasks:\n  - { name: a, on: [{ issue: [] }], context: { commands: "+
		"[{ name: search, run: rg TODO }, { name: fetch, run: curl example.com }] } }\n"), op)
	if err != nil {
		t.Fatal(err)
	}
	cs := m.Tasks[0].Context.Commands
	if len(cs) != 1 || cs[0].Name != "search" || len(m.TaskNotes) != 1 ||
		m.TaskNotes[0].String() != `task a: context.commands.fetch: "curl" was dropped; allowed: rg` {
		t.Fatalf("commands %+v, notes %v", cs, m.TaskNotes)
	}
}
