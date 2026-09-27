package configfile

import (
	"reflect"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/tasks"
)

func TestTasks(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	const head = `providers:
  p: { type: openai, apiKey: { env: TEST_WEBHOOK_SECRET } }
defaults:
  tasks:
    - { name: triage, on: [{ issue: [opened] }], actions: { labels: { propose: { add: [bug] } } } }
  allow:
    tasks: { enabled: true, events: ["issue.*"], maxTasks: 3 }
`
	doc := func(tenantKeys, repos string) string {
		return head + strings.Replace(minimal, "slug: acme", "slug: acme\n"+tenantKeys+"    repositories: ["+repos+"]", 1)
	}

	f, err := Parse([]byte(doc("    allow: { tasks: { actions: [comment, state] } }\n",
		"{ name: acme/x, tasks: [{ name: other, on: [{ comment: [] }] }], allow: { tasks: { maxTasks: 1, repositoryTasks: false } } }")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tenant := f.Settings(&f.Tenants[0], "", "")
	if len(tenant.Tasks) != 1 || tenant.Tasks[0].Name != "triage" {
		t.Fatalf("tenant tasks = %+v", tenant.Tasks)
	}
	want := tasks.Bounds{
		Enabled: true, Events: []string{"issue.*"}, Actions: []string{"comment", "state"}, Context: tasks.DefaultContext,
		Tools: tasks.DefaultTools, RepositoryTasks: true, MaxTasks: 3, MaxRunsPerSubjectPerHour: tasks.DefaultMaxRunsPerSubjectPerHour, MaxFields: tasks.DefaultMaxFields,
	}
	if !reflect.DeepEqual(tenant.TaskBounds, want) {
		t.Fatalf("tenant bounds = %+v, want %+v", tenant.TaskBounds, want)
	}
	repo := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
	want.MaxTasks, want.RepositoryTasks = 1, false
	if len(repo.Tasks) != 2 || repo.Tasks[1].Name != "other" || !reflect.DeepEqual(repo.TaskBounds, want) {
		t.Fatalf("repository tasks = %+v, bounds %+v", repo.Tasks, repo.TaskBounds)
	}
	if s := f.Sources(&f.Tenants[0], "acme-bot", "acme/x"); s["tasks"] != SourceFile {
		t.Fatalf("tasks from %s", s["tasks"])
	}

	tests := []struct {
		name, yaml, want string
	}{
		{"a task that does not check", doc("    tasks: [{ name: Bad, on: [{ issue: [] }] }]\n", ""), "tenants[0].tasks[0]: name"},
		{"two tasks with one name", doc("    tasks: [{ name: a, on: [{ issue: [] }] }, { name: a, on: [{ comment: [] }] }]\n", ""),
			"tasks[1]: name \"a\" is used twice"},
		{"a task model of an undeclared provider", doc("    tasks: [{ name: a, on: [{ issue: [] }], models: { review: q/big } }]\n", ""),
			"tasks[0].models.review references provider \"q\""},
		{"an unknown task key", doc("    tasks: [{ name: a, on: [{ issue: [] }], bogus: 1 }]\n", ""), "bogus"},
		{"an unknown action kind", doc("    allow: { tasks: { actions: [delete] } }\n", ""), "allow.tasks.actions[0] must be one of"},
		{"an unknown context kind", doc("    allow: { tasks: { context: [web] } }\n", ""), "allow.tasks.context[0] must be one of"},
		{"a bad event glob", doc("    allow: { tasks: { events: ['['] } }\n", ""), "allow.tasks.events[0]"},
		{"a bad tool name", doc("    allow: { tasks: { tools: [Read-File] } }\n", ""), "allow.tasks.tools[0]"},
		{"a cap that is not positive", doc("    allow: { tasks: { maxFields: 0 } }\n", ""), "allow.tasks.maxFields must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

// TestDashboardTenantTasks checks a dashboard tenant's JSON spec carries
// tasks, fields in order, with the file's strictness.
func TestDashboardTenantTasks(t *testing.T) {
	t.Parallel()
	spec := `{"slug":"beta","installations":[{"name":"b"}],"tasks":[{"name":"a","on":[{"issue":["opened"]},{"raw":{"event":"release"}}],` +
		`"fields":{"z":{"type":"string"},"a":{"type":"boolean"}}}]}`
	tn, err := DecodeTenant(DashboardTenant{Slug: "beta", Spec: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	task := tn.Tasks[0]
	if len(task.On) != 2 || task.On[1].RawEvent != "release" || task.Fields[0].Name != "z" {
		t.Fatalf("task = %+v", task)
	}
	bad := strings.Replace(spec, `"type":"boolean"`, `"type":"boolean","bogus":1`, 1)
	if _, err := DecodeTenant(DashboardTenant{Slug: "beta", Spec: []byte(bad)}); err == nil {
		t.Fatal("an unknown field key decoded")
	}
}

func TestTaskBoundsDefaults(t *testing.T) {
	t.Parallel()
	got := (*TaskBounds)(nil).Resolve()
	want := tasks.Bounds{
		Events: tasks.DefaultEvents, Actions: tasks.DefaultActions, Context: tasks.DefaultContext, Tools: tasks.DefaultTools,
		RepositoryTasks: true, MaxTasks: tasks.DefaultMaxTasks, MaxRunsPerSubjectPerHour: tasks.DefaultMaxRunsPerSubjectPerHour, MaxFields: tasks.DefaultMaxFields,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
	got.Events[0], got.Actions[0], got.Context[0], got.Tools[0] = "x", "x", "x", "x"
	if tasks.DefaultEvents[0] == "x" || tasks.DefaultActions[0] == "x" || tasks.DefaultContext[0] == "x" || tasks.DefaultTools[0] == "x" {
		t.Fatal("Resolve handed out the package defaults")
	}
}

// TestTasksMergeByName checks the operator's tasks merge across scopes by
// name, that enabled false switches an inherited one off, and that a
// dashboard tenant's tasks are kept apart from the file's.
func TestTasksMergeByName(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f, err := Parse([]byte(`defaults:
  tasks:
    - { name: triage, on: [{ issue: [] }] }
    - { name: stale, on: [{ issue: [] }] }
    - { name: welcome, on: [{ pull_request: [] }] }
` + strings.Replace(minimal, "slug: acme", `slug: acme
    tasks:
      - { name: triage, on: [{ comment: [] }] }
      - { name: extra, on: [{ issue: [] }] }
    repositories:
      - { name: acme/x, tasks: [{ name: stale, enabled: false }, { name: welcome, mode: single, on: [{ pull_request: [opened] }] }] }`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
	got := make([]string, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		got = append(got, t.Name+":"+t.On[0].Event+":"+string(t.Mode))
	}
	if strings.Join(got, ",") != "triage:comment:,welcome:pull_request:single,extra:issue:" || s.DashboardTasks != nil {
		t.Fatalf("tasks %v, dashboard %v", got, s.DashboardTasks)
	}

	dash, err := DecodeTenant(DashboardTenant{Slug: "beta", Spec: []byte(`{"slug":"beta","installations":[{"name":"b"}],` +
		`"tasks":[{"name":"triage","on":[{"comment":[]}]},{"name":"mine","on":[{"issue":[]}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	s = f.Settings(&dash, "", "")
	if len(s.Tasks) != 2 || s.Tasks[0].Name != "stale" || s.Tasks[1].Name != "welcome" ||
		len(s.DashboardTasks) != 2 || s.DashboardTasks[0].Name != "triage" || s.DashboardTasks[1].Name != "mine" {
		t.Fatalf("file tasks %+v, dashboard tasks %+v", s.Tasks, s.DashboardTasks)
	}
}
