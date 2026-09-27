package tasks

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// triage is the plan's issue triage example.
const triage = `
name: triage
on:
  - issue: [opened, reopened]
  - comment: [created]
  - raw: {event: "issues", actions: ["transferred"]}
if: 'subject.kind == "issue" && !("triaged" in subject.labels)'
mode: agentic
models: {review: anthropic/claude}
agent: {maxSteps: 20, timeout: 5m, tools: [read_file, grep, list_files], commands: [rg]}
context:
  thread: {comments: 30}
  files: [{path: CONTRIBUTING.md}, {glob: "docs/area/*.md", max: 5}]
  search: [{name: code, query: "{{ .Subject.Title }}", k: 8}]
  related: [{name: dupes, query: "is:open {{ .Subject.Title }}", k: 5}]
  commands: [{name: owners, run: "cat .github/CODEOWNERS"}]
system: .kritik/tasks/system.md.tmpl
prompt: .kritik/tasks/triage.md.tmpl
fields:
  priority: {type: string, enum: [p0, p1, p2, p3]}
  area:     {type: string, enum: [api, web, runner, docs]}
  needsInfo: {type: boolean}
  missing:  {type: array, items: {type: string, maxLength: 200}, maxItems: 5}
actions:
  comment:
    mode: sticky
    template: .kritik/tasks/triage-comment.md.tmpl
    if: 'answer.fields.needsInfo || size(answer.labels.add) > 0'
  labels:
    propose: {add: ["bug", "enhancement", "area/*"], remove: ["needs-triage"]}
    rules:
      - add: ["priority/{{ .Fields.priority }}", "area/{{ .Fields.area }}"]
      - add: ["needs-info"]
        if: 'answer.fields.needsInfo'
  state:     {propose: {close: true}, if: 'subject.kind == "issue"'}
  assign:    {propose: {users: ["alice", "bob"]}}
  reviewers: {rules: [{users: ["bob"], if: 'answer.fields.area == "runner"'}]}
  inlineComments: {propose: true}
`

// triageFiles are the files the triage example names.
func triageFiles() map[string][]byte {
	return map[string][]byte{
		".kritik/tasks/system.md.tmpl":         []byte("Triage for {{ .Repo.Owner }}/{{ .Repo.Name }}."),
		".kritik/tasks/triage.md.tmpl":         []byte("Triage {{ .Subject.Kind }} #{{ .Subject.Number }}: {{ .Subject.Title }}"),
		".kritik/tasks/triage-comment.md.tmpl": []byte("{{ .Answer.Summary }} (priority {{ .Fields.priority }}; labels {{ join \", \" .Applied.AddLabels }})"),
	}
}

func decode(t *testing.T, doc string) (Task, error) {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	var task Task
	err := dec.Decode(&task)
	return task, err
}

func mustTask(t *testing.T, doc string) *Task {
	t.Helper()
	task, err := decode(t, doc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := task.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	return &task
}

func TestDecode(t *testing.T) {
	t.Parallel()
	task := mustTask(t, triage)
	want := []Trigger{
		{Event: EventIssue, Actions: []string{"opened", "reopened"}},
		{Event: EventComment, Actions: []string{"created"}},
		{Event: EventRaw, RawEvent: "issues", Actions: []string{"transferred"}},
	}
	if !reflect.DeepEqual(task.On, want) {
		t.Fatalf("on = %+v, want %+v", task.On, want)
	}
	names := make([]string, 0, len(task.Fields))
	for _, f := range task.Fields {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "priority,area,needsInfo,missing" {
		t.Fatalf("fields in order %v", names)
	}
	if got := task.Files(); !reflect.DeepEqual(got, []string{".kritik/tasks/system.md.tmpl", ".kritik/tasks/triage.md.tmpl",
		".kritik/tasks/triage-comment.md.tmpl"}) {
		t.Fatalf("Files() = %v", got)
	}

	raw, err := yaml.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decode(t, string(raw))
	if err != nil {
		t.Fatalf("decode the marshalled task: %v\n%s", err, raw)
	}
	if !reflect.DeepEqual(&again, task) {
		t.Fatalf("round trip changed the task:\n%s", raw)
	}
}

func TestDecode_Strict(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"unknown task key":         "name: a\non: [{issue: []}]\nbogus: 1\n",
		"unknown field key":        "name: a\non: [{issue: []}]\nfields: {x: {type: string, bogus: 1}}\n",
		"unknown nested field key": "name: a\non: [{issue: []}]\nfields: {x: {type: array, items: {type: string, bogus: 1}}}\n",
		"unknown raw key":          "name: a\non: [{raw: {event: release, bogus: 1}}]\n",
		"two trigger keys":         "name: a\non: [{issue: [], comment: []}]\n",
		"unknown trigger":          "name: a\non: [{release: []}]\n",
		"unknown action key":       "name: a\non: [{issue: []}]\nactions: {labels: {propose: {add: [x], bogus: 1}}}\n",
		"fields not a mapping":     "name: a\non: [{issue: []}]\nfields: [x]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := decode(t, doc); err == nil {
				t.Fatal("decode succeeded")
			}
		})
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	const base = "name: a\non: [{issue: [opened]}]\n"
	tests := []struct {
		name, doc, want string
	}{
		{"bad name", "name: Triage\non: [{issue: []}]\n", "name"},
		{"no trigger", "name: a\n", "at least one trigger"},
		{"bad CEL", base + "if: 'subject.kind =='\n", "if"},
		{"CEL not a boolean", base + "if: '1 + 1'\n", "boolean"},
		{"answer in the task's if", base + "if: 'answer.summary != \"\"'\n", "cannot use answer"},
		{"bad mode", base + "mode: fast\n", "mode"},
		{"bad model", base + "models: {review: claude}\n", "models.review"},
		{"negative agent limit", base + "agent: {maxSteps: 0}\n", "positive"},
		{"command path", base + "agent: {commands: [/bin/sh]}\n", "bare command"},
		{"bad raw glob", "name: a\non: [{raw: {event: '['}}]\n", "raw.event"},
		{"bad action glob", "name: a\non: [{issue: ['[']}]\n", "actions[0]"},
		{"field name", base + "fields: {Priority: {type: string}}\n", "field name"},
		{"field type", base + "fields: {x: {type: date}}\n", "type must be"},
		{"enum on a number", base + "fields: {x: {type: number, enum: [a]}}\n", "apply to a string"},
		{"array without items", base + "fields: {x: {type: array}}\n", "needs items"},
		{"nested array", base + "fields: {x: {type: array, items: {type: array, items: {type: string}}}}\n", "must be string"},
		{"object of objects", base + "fields: {x: {type: object, properties: {y: {type: object, properties: {z: {type: string}}}}}}\n",
			"must be string"},
		{"bad pattern", base + "fields: {x: {type: string, pattern: '('}}\n", "pattern"},
		{"maxLength too large", base + "fields: {x: {type: string, maxLength: 100000}}\n", "maxLength"},
		{"minimum above maximum", base + "fields: {x: {type: integer, minimum: 5, maximum: 1}}\n", "minimum"},
		{"duplicate enum", base + "fields: {x: {type: string, enum: [a, a]}}\n", "distinct"},
		{"absolute prompt", base + "prompt: /etc/passwd\n", "relative"},
		{"escaping system", base + "system: ../x\n", "escapes"},
		{"escaping comment template", base + "actions: {comment: {template: ../../x}}\n", "escapes"},
		{"escaping context file", base + "context: {files: [{path: ../x}]}\n", "escapes"},
		{"escaping context glob", base + "context: {files: [{glob: '../**'}]}\n", "glob"},
		{"prompt and promptInline", base + "prompt: a.md\npromptInline: hi\n", "not both"},
		{"bad promptInline", base + "promptInline: '{{ .Subject'\n", "promptInline"},
		{"define in promptInline", base + "promptInline: '{{ define \"x\" }}{{ end }}'\n", "define"},
		{"bad rule template", base + "actions: {labels: {rules: [{add: ['{{ .Fields.x']}]}}\n", "labels.rules[0].add"},
		{"bad action CEL", base + "actions: {labels: {propose: {add: [x]}, if: 'answer.'}}\n", "if"},
		{"state rule both", base + "actions: {state: {rules: [{close: true, reopen: true}]}}\n", "exactly one"},
		{"bad propose login", base + "actions: {assign: {propose: {users: ['a b']}}}\n", "not a login"},
		{"commands in single mode", base + "mode: single\ncontext: {commands: [{name: x, run: ls}]}\n", "agentic"},
		{"a command path", base + "context: {commands: [{name: x, run: /bin/sh -c id}]}\n", "bare command name"},
		{"a shell line", base + "context: {commands: [{name: x, run: 'cat a | sh'}]}\n", "not given to a shell"},
		{"a substitution", base + "context: {commands: [{name: x, run: 'echo $(id)'}]}\n", "not given to a shell"},
		{"a switched-off task still needs a good name", "name: Bad\nenabled: false\n", "name"},
		{"duplicate context name", base + "context: {search: [{name: x, query: a}], related: [{name: x, query: b}]}\n", "twice"},
		{"duplicate related and command name", base + "context: {related: [{name: x, query: a}], commands: [{name: x, run: ls}]}\n", "twice"},
		{"a search named files", base + "context: {search: [{name: files, query: a}]}\n", "reserved"},
		{"a related source named files", base + "context: {related: [{name: files, query: a}]}\n", "reserved"},
		{"a command named files", base + "context: {commands: [{name: files, run: ls}]}\n", "reserved"},
		{"a command named notes", base + "context: {commands: [{name: notes, run: ls}]}\n", "reserved"},
		{"subject-bound action on a raw release trigger", "name: a\non: [{raw: {event: release}}]\nactions: {labels: {propose: {add: [x]}}}\n",
			"can fire without one"},
		{"inline comments on issues only", "name: a\non: [{issue: []}, {raw: {event: issues}}]\nactions: {inlineComments: {propose: true}}\n",
			"pull request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task, err := decode(t, tt.doc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			err = task.Check()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Check() = %v, want an error containing %q", err, tt.want)
			}
		})
	}

	for name, doc := range map[string]string{
		"comment mode none on a raw release trigger": "name: a\non: [{raw: {event: release}}]\nactions: {comment: {mode: none}}\n",
		"a raw issues trigger writes labels":         "name: a\non: [{raw: {event: issues}}]\nactions: {labels: {propose: {add: [x]}}}\n",
		"inline comments with a comment trigger":     "name: a\non: [{issue: []}, {comment: []}]\nactions: {inlineComments: {propose: true}}\n",
		"answer in an action's if":                   base + "actions: {labels: {propose: {add: [x]}, if: 'answer.fields.x == 1'}}\n",
		"single mode":                                base + "mode: single\n",
		"a switched-off task needs only its name":    "name: a\nenabled: false\n",
	} {
		t.Run("valid: "+name, func(t *testing.T) {
			t.Parallel()
			mustTask(t, doc)
		})
	}
}

func TestMatches(t *testing.T) {
	t.Parallel()
	task := mustTask(t, triage)
	issue := SampleInput()
	labelled := SampleInput()
	labelled.Subject.Labels = []string{"triaged"}
	transferred := SampleInput()
	transferred.Event, transferred.Action = "", "transferred"
	pull := SampleInput()
	pull.Event, pull.Subject.Kind = EventPullRequest, SubjectPull
	closed := SampleInput()
	closed.Action = "closed"
	release := Input{Forge: "github", RawEvent: "release", Action: "published", Raw: map[string]any{"release": map[string]any{"tag_name": "v1"}}}
	releases := mustTask(t, "name: rel\non: [{raw: {event: 'rel*', actions: [pub*]}}]\nif: 'raw.release.tag_name.startsWith(\"v\")'\n")
	anyIssue := mustTask(t, "name: any\non: [{issue: []}]\n")
	tests := []struct {
		name string
		task *Task
		in   Input
		want bool
	}{
		{"a normalized trigger", task, issue, true},
		{"the if rejects", task, labelled, false},
		{"a raw trigger without a normalized event", task, transferred, true},
		{"another event", task, pull, false},
		{"another action", task, closed, false},
		{"a raw glob and the payload", releases, release, true},
		{"a raw glob that does not match", releases, issue, false},
		{"any action", anyIssue, closed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.task.Matches(tt.in)
			if err != nil || got != tt.want {
				t.Fatalf("Matches() = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
	t.Run("a failing if is an error", func(t *testing.T) {
		t.Parallel()
		bad := mustTask(t, "name: a\non: [{issue: []}]\nif: 'raw.missing.key == 1'\n")
		if _, err := bad.Matches(issue); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestClip(t *testing.T) {
	t.Parallel()
	full := Bounds{
		Enabled: true, Events: DefaultEvents, Actions: DefaultActions, Context: DefaultContext, Tools: DefaultTools, RepositoryTasks: true,
		MaxTasks: DefaultMaxTasks, MaxFields: DefaultMaxFields,
	}
	task := *mustTask(t, triage)
	names := func(ts []Task) string {
		out := make([]string, 0, len(ts))
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return strings.Join(out, ",")
	}
	t.Run("disabled drops every task", func(t *testing.T) {
		t.Parallel()
		kept, notes := Clip([]Task{task}, Bounds{}, false)
		if len(kept) != 0 || len(notes) != 1 || notes[0].Reason != "tasks are disabled" {
			t.Fatalf("kept %v, notes %v", names(kept), notes)
		}
	})
	t.Run("the operator's tasks are kept whole", func(t *testing.T) {
		t.Parallel()
		kept, notes := Clip([]Task{task}, Bounds{Enabled: true}, false)
		if len(notes) != 0 || !reflect.DeepEqual(kept, []Task{task}) {
			t.Fatalf("kept %+v, notes %v", kept, notes)
		}
	})
	t.Run("repository tasks off", func(t *testing.T) {
		t.Parallel()
		b := full
		b.RepositoryTasks = false
		if kept, _ := Clip([]Task{task}, b, true); len(kept) != 0 {
			t.Fatal("kept a repository task")
		}
	})
	t.Run("over maxTasks", func(t *testing.T) {
		t.Parallel()
		b := full
		b.MaxTasks = 1
		other := task
		other.Name = "other"
		kept, notes := Clip([]Task{task, other}, b, true)
		if names(kept) != "triage" || len(notes) == 0 || notes[len(notes)-1].Task != "other" {
			t.Fatalf("kept %v, notes %v", names(kept), notes)
		}
	})
	t.Run("a trigger keeps its allowed actions", func(t *testing.T) {
		t.Parallel()
		b := full
		b.Events = []string{"issue.opened"}
		kept, _ := Clip([]Task{*mustTask(t, "name: a\non: [{issue: [opened, closed]}, {issue: []}]\n")}, b, true)
		if len(kept) != 1 || !reflect.DeepEqual(kept[0].On, []Trigger{{Event: EventIssue, Actions: []string{"opened"}}}) {
			t.Fatalf("kept %+v", kept)
		}
	})
	t.Run("a switched-off task", func(t *testing.T) {
		t.Parallel()
		kept, notes := Clip([]Task{*mustTask(t, "name: a\nenabled: false\n")}, full, false)
		if len(kept) != 0 || notes[0].Reason != "the task is switched off" {
			t.Fatalf("kept %v, notes %v", names(kept), notes)
		}
	})
	t.Run("tools outside the bounds", func(t *testing.T) {
		t.Parallel()
		kept, notes := Clip([]Task{*mustTask(t, "name: a\non: [{issue: []}]\nagent: {tools: [grep, run, read_file]}\n")}, full, true)
		if len(kept) != 1 || !reflect.DeepEqual(kept[0].Agent.Tools, []string{"grep", "read_file"}) ||
			len(notes) != 1 || notes[0].What != "agent.tools run" {
			t.Fatalf("kept %+v, notes %v", kept, notes)
		}
		kept, _ = Clip([]Task{*mustTask(t, "name: a\non: [{issue: []}]\nagent: {tools: [run]}\n")}, full, true)
		if kept[0].Agent.Tools == nil || len(kept[0].Agent.Tools) != 0 {
			t.Fatalf("no allowed tool left must be none, not every tool: %#v", kept[0].Agent.Tools)
		}
	})
	t.Run("no trigger left", func(t *testing.T) {
		t.Parallel()
		kept, notes := Clip([]Task{*mustTask(t, "name: a\non: [{raw: {event: release}}]\n")}, full, true)
		if len(kept) != 0 || notes[len(notes)-1].Reason != "no trigger is left" {
			t.Fatalf("kept %v, notes %v", names(kept), notes)
		}
	})
	t.Run("a raw event the operator lists", func(t *testing.T) {
		t.Parallel()
		b := full
		b.Events = []string{"raw:release.*"}
		if kept, _ := Clip([]Task{*mustTask(t, "name: a\non: [{raw: {event: release, actions: [published]}}]\n")}, b, true); len(kept) != 1 {
			t.Fatal("dropped an allowed raw trigger")
		}
	})
}

func TestClip_Defaults(t *testing.T) {
	t.Parallel()
	task := *mustTask(t, triage)
	b := Bounds{
		Enabled: true, Events: DefaultEvents, Actions: DefaultActions, Context: DefaultContext, Tools: DefaultTools, RepositoryTasks: true,
		MaxTasks: DefaultMaxTasks, MaxFields: 2,
	}
	kept, notes := Clip([]Task{task}, b, true)
	if len(kept) != 1 {
		t.Fatalf("kept %d tasks, notes %v", len(kept), notes)
	}
	k := kept[0]
	if len(k.On) != 2 {
		t.Errorf("on = %+v, want the raw trigger dropped", k.On)
	}
	if k.Actions.State != nil || k.Actions.Assign != nil || k.Actions.Reviewers != nil {
		t.Error("state, assign and reviewers are not allowed by default")
	}
	if k.Actions.Labels == nil || k.Actions.Comment == nil || k.Actions.InlineComments == nil {
		t.Error("comment, labels and inlineComments are allowed by default")
	}
	if k.Context.Commands != nil || k.Context.Thread == nil {
		t.Error("commands are not allowed by default; thread is")
	}
	if k.System != "" {
		t.Error("the system prompt is not allowed by default")
	}
	if len(k.Fields) != 2 || len(task.Fields) != 4 {
		t.Errorf("fields = %d, want 2, and the input untouched", len(k.Fields))
	}
	if task.Actions.State == nil || task.System == "" {
		t.Error("Clip modified its input")
	}
	what := make([]string, 0, len(notes))
	for _, n := range notes {
		what = append(what, n.What)
	}
	want := "on[2] raw:issues.transferred,actions.state,actions.assign,actions.reviewers,context.commands,system,fields.needsInfo,fields.missing"
	if strings.Join(what, ",") != want {
		t.Errorf("notes %v, want %s", what, want)
	}
}

func TestStickyMarker(t *testing.T) {
	t.Parallel()
	if got := StickyMarker("triage"); got != "<!-- kritik:task:triage -->" {
		t.Fatalf("StickyMarker = %q", got)
	}
}

func TestYAMLOfFieldsKeepsOrder(t *testing.T) {
	t.Parallel()
	task := mustTask(t, triage)
	raw, err := yaml.Marshal(task.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if i, j := bytes.Index(raw, []byte("priority")), bytes.Index(raw, []byte("missing")); i < 0 || j < i {
		t.Fatalf("fields out of order:\n%s", raw)
	}
}

func TestBudget(t *testing.T) {
	t.Parallel()
	b := &Budget{PerSource: 5, Left: 8}
	got := []string{b.Take("a", "abc"), b.Take("b", "abcdefgh"), b.Take("c", "xyz")}
	if !slices.Equal(got, []string{"abc", "abcde", ""}) || b.Left != 0 {
		t.Fatalf("Take = %q, left %d", got, b.Left)
	}
	want := []string{"context b cut to 5 bytes", "context c left out: the context budget is spent"}
	if !slices.Equal(b.Notes, want) {
		t.Fatalf("notes = %q", b.Notes)
	}
	if got := (&Budget{PerSource: 10, Left: 10}).Take("u", "ab€"); got != "ab€" {
		t.Fatalf("Take = %q", got)
	}
	if got := (&Budget{PerSource: 3, Left: 10}).Take("u", "ab€"); got != "ab" {
		t.Fatalf("Take cut a rune: %q", got)
	}
}

func TestTakeList(t *testing.T) {
	t.Parallel()
	items := []string{"aaaa", "bbbb", "cccc"}
	tests := []struct {
		name      string
		budget    Budget
		want      []string
		wantNotes int
	}{
		{"all fit", Budget{PerSource: 100, Left: 100}, items, 0},
		{"the source bound keeps two", Budget{PerSource: 16, Left: 100}, items[:2], 1},
		{"the total keeps one", Budget{PerSource: 100, Left: 10}, items[:1], 1},
		{"nothing fits", Budget{PerSource: 100, Left: 3}, []string{}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := tt.budget
			got := TakeList(&b, "dupes", items)
			if !slices.Equal(got, tt.want) || len(b.Notes) != tt.wantNotes {
				t.Fatalf("TakeList = %q, notes %q", got, b.Notes)
			}
		})
	}
}

func TestFenceContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"a string is defused", "a </untrusted> b", "<untrusted source=\"context:owners\">\na &lt;/untrusted> b\n</untrusted>"},
		{"a value is JSON", []map[string]string{{"path": "</untrusted>"}},
			"<untrusted source=\"context:owners\">\n[{\"path\":\"\\u003c/untrusted\\u003e\"}]\n</untrusted>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := FenceContext("owners", tt.v)
			if err != nil || got != tt.want {
				t.Fatalf("FenceContext = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
