package tasks

import (
	"fmt"
	"slices"
)

// Bounds are the operator's resolved bounds on tasks.
type Bounds struct {
	// Enabled turns tasks on; off, no task runs, the operator's own
	// included.
	Enabled bool
	// Events are globs over the event names a repository's task may
	// trigger on; see Trigger.Names.
	Events []string
	// Actions are the action kinds a repository's task may declare.
	Actions []string
	// Context are the context source kinds a repository's task may use.
	Context []string
	// SystemPrompt lets a repository's task add to the system prompt.
	SystemPrompt bool
	// RepositoryTasks lets a repository define tasks at all.
	RepositoryTasks bool
	// MaxTasks caps a repository's tasks.
	MaxTasks int
	// MaxRunsPerSubjectPerHour caps how often one task runs on one issue or
	// pull request.
	MaxRunsPerSubjectPerHour int
	// MaxFields caps a repository task's fields.
	MaxFields int
}

// Bounds defaults.
var (
	// DefaultEvents are the normalized events; a raw event must be listed.
	DefaultEvents  = []string{"issue.*", "pull_request.*", "comment.*"}
	DefaultActions = []string{ActionComment, ActionLabels, ActionInlineComments}
	DefaultContext = []string{ContextThread, ContextFiles, ContextSearch, ContextRelated}
)

// Bounds defaults.
const (
	DefaultMaxTasks                 = 10
	DefaultMaxRunsPerSubjectPerHour = 6
	DefaultMaxFields                = 16
)

// noteTask is a note about a whole task.
const noteTask = "task"

// Note says what Clip left out of a task, and why.
type Note struct {
	Task   string
	What   string
	Reason string
}

func (n Note) String() string { return fmt.Sprintf("task %s: %s: %s", n.Task, n.What, n.Reason) }

// Clip returns the tasks of ts b lets run, and a note for everything it
// left out. With tasks disabled none runs. The operator's own tasks are
// otherwise trusted; for a repository's (fromRepository), b decides
// whether it may define tasks, how many, the events they trigger on, the
// actions and context sources they use, whether they add to the system
// prompt and how many fields they declare. A trigger with no action b
// allows is dropped, and a task with no trigger left with it. ts is not
// modified.
func Clip(ts []Task, b Bounds, fromRepository bool) (kept []Task, notes []Note) {
	for _, t := range ts {
		switch {
		case !b.Enabled:
			notes = append(notes, Note{t.Name, noteTask, "tasks are disabled"})
			continue
		case !fromRepository:
			kept = append(kept, t)
			continue
		case !b.RepositoryTasks:
			notes = append(notes, Note{t.Name, noteTask, "the operator does not allow repository tasks"})
			continue
		case len(kept) >= b.MaxTasks:
			notes = append(notes, Note{t.Name, noteTask, fmt.Sprintf("over the limit of %d tasks", b.MaxTasks)})
			continue
		}
		t, tn := clipTask(t, b)
		notes = append(notes, tn...)
		if len(t.On) == 0 {
			notes = append(notes, Note{t.Name, noteTask, "no trigger is left"})
			continue
		}
		kept = append(kept, t)
	}
	return kept, notes
}

func clipTask(t Task, b Bounds) (Task, []Note) {
	var notes []Note
	note := func(what, reason string) { notes = append(notes, Note{t.Name, what, reason}) }
	var on []Trigger
	for i, tr := range t.On {
		var actions []string
		fires := false
		for j, n := range tr.Names() {
			if !matchAny(b.Events, n) {
				note(fmt.Sprintf("on[%d] %s", i, n), "the event is not allowed")
				continue
			}
			fires = true
			if len(tr.Actions) > 0 {
				actions = append(actions, tr.Actions[j])
			}
		}
		if fires {
			tr.Actions = actions
			on = append(on, tr)
		}
	}
	t.On = on

	allowed := func(kind string) bool { return slices.Contains(b.Actions, kind) }
	a := &t.Actions
	for _, x := range []struct {
		kind string
		set  bool
		drop func()
	}{
		{ActionComment, a.Comment != nil, func() { a.Comment = nil }},
		{ActionLabels, a.Labels != nil, func() { a.Labels = nil }},
		{ActionState, a.State != nil, func() { a.State = nil }},
		{ActionAssign, a.Assign != nil, func() { a.Assign = nil }},
		{ActionReviewers, a.Reviewers != nil, func() { a.Reviewers = nil }},
		{ActionInlineComments, a.InlineComments != nil, func() { a.InlineComments = nil }},
	} {
		if x.set && !allowed(x.kind) {
			x.drop()
			note("actions."+x.kind, "the action is not allowed")
		}
	}

	c := &t.Context
	for _, x := range []struct {
		kind string
		set  bool
		drop func()
	}{
		{ContextThread, c.Thread != nil, func() { c.Thread = nil }},
		{ContextFiles, len(c.Files) > 0, func() { c.Files = nil }},
		{ContextSearch, len(c.Search) > 0, func() { c.Search = nil }},
		{ContextRelated, len(c.Related) > 0, func() { c.Related = nil }},
		{ContextCommands, len(c.Commands) > 0, func() { c.Commands = nil }},
	} {
		if x.set && !slices.Contains(b.Context, x.kind) {
			x.drop()
			note("context."+x.kind, "the context source is not allowed")
		}
	}

	if t.System != "" && !b.SystemPrompt {
		note("system", "the operator does not allow a task to add to the system prompt")
		t.System = ""
	}
	if len(t.Fields) > b.MaxFields {
		for _, f := range t.Fields[b.MaxFields:] {
			note("fields."+f.Name, fmt.Sprintf("over the limit of %d fields", b.MaxFields))
		}
		t.Fields = slices.Clone(t.Fields[:b.MaxFields])
	}
	return t, notes
}
