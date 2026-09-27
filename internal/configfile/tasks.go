package configfile

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/home-operations/kritik/internal/tasks"
)

// TaskBounds bound tasks (ADR-0012), bound by bound like the rest of
// Allow: one written at a narrower scope replaces the broader scope's. A
// bound written nowhere takes its default; tasks are off unless enabled.
type TaskBounds struct {
	// Enabled turns tasks on, the operator's own included.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Events are globs over the event names a repository's task may
	// trigger on, such as "issue.*" or "raw:release.*"; default the
	// normalized events, so a raw event must be listed.
	Events []string `yaml:"events,omitempty"`
	// Actions are the action kinds a repository's task may declare;
	// default comment, labels and inlineComments, so state, assign and
	// reviewers must be listed.
	Actions []string `yaml:"actions,omitempty"`
	// Context are the context source kinds a repository's task may use;
	// default all but commands.
	Context []string `yaml:"context,omitempty"`
	// Tools are the agent tools a repository's task may use; default the
	// read-only read_file, grep and list_files.
	Tools []string `yaml:"tools,omitempty"`
	// SystemPrompt lets a repository's task add to the system prompt;
	// default false.
	SystemPrompt *bool `yaml:"systemPrompt,omitempty"`
	// RepositoryTasks lets a repository define tasks; default true.
	RepositoryTasks *bool `yaml:"repositoryTasks,omitempty"`
	// MaxTasks caps a repository's tasks; default tasks.DefaultMaxTasks.
	MaxTasks *int `yaml:"maxTasks,omitempty"`
	// MaxRunsPerSubjectPerHour caps how often one task runs on one issue or
	// pull request; default tasks.DefaultMaxRunsPerSubjectPerHour.
	MaxRunsPerSubjectPerHour *int `yaml:"maxRunsPerSubjectPerHour,omitempty"`
	// MaxFields caps a repository task's fields; default
	// tasks.DefaultMaxFields.
	MaxFields *int `yaml:"maxFields,omitempty"`
}

// taskActionKinds and taskContextKinds are what the bounds may list.
var (
	taskActionKinds = []string{
		tasks.ActionComment, tasks.ActionLabels, tasks.ActionState, tasks.ActionAssign, tasks.ActionReviewers, tasks.ActionInlineComments,
	}
	taskContextKinds = []string{tasks.ContextThread, tasks.ContextFiles, tasks.ContextSearch, tasks.ContextRelated, tasks.ContextCommands}
)

// Resolve is the bounds with every default applied; b may be nil.
func (b *TaskBounds) Resolve() tasks.Bounds {
	if b == nil {
		b = &TaskBounds{}
	}
	pick := func(v *int, def int) int {
		if v != nil {
			return *v
		}
		return def
	}
	out := tasks.Bounds{
		Enabled: b.Enabled != nil && *b.Enabled, Events: slices.Clone(b.Events), Actions: slices.Clone(b.Actions),
		Context: slices.Clone(b.Context), Tools: slices.Clone(b.Tools),
		SystemPrompt: b.SystemPrompt != nil && *b.SystemPrompt, RepositoryTasks: b.RepositoryTasks == nil || *b.RepositoryTasks,
		MaxTasks:                 pick(b.MaxTasks, tasks.DefaultMaxTasks),
		MaxRunsPerSubjectPerHour: pick(b.MaxRunsPerSubjectPerHour, tasks.DefaultMaxRunsPerSubjectPerHour),
		MaxFields:                pick(b.MaxFields, tasks.DefaultMaxFields),
	}
	if out.Events == nil {
		out.Events = slices.Clone(tasks.DefaultEvents)
	}
	if out.Actions == nil {
		out.Actions = slices.Clone(tasks.DefaultActions)
	}
	if out.Context == nil {
		out.Context = slices.Clone(tasks.DefaultContext)
	}
	if out.Tools == nil {
		out.Tools = slices.Clone(tasks.DefaultTools)
	}
	return out
}

// overlay lays the bounds o writes over b's.
func (b *TaskBounds) overlay(o *TaskBounds) *TaskBounds {
	if o == nil {
		return b
	}
	out := TaskBounds{}
	if b != nil {
		out = *b
	}
	if o.Enabled != nil {
		out.Enabled = o.Enabled
	}
	if o.Events != nil {
		out.Events = o.Events
	}
	if o.Actions != nil {
		out.Actions = o.Actions
	}
	if o.Context != nil {
		out.Context = o.Context
	}
	if o.Tools != nil {
		out.Tools = o.Tools
	}
	if o.SystemPrompt != nil {
		out.SystemPrompt = o.SystemPrompt
	}
	if o.RepositoryTasks != nil {
		out.RepositoryTasks = o.RepositoryTasks
	}
	if o.MaxTasks != nil {
		out.MaxTasks = o.MaxTasks
	}
	if o.MaxRunsPerSubjectPerHour != nil {
		out.MaxRunsPerSubjectPerHour = o.MaxRunsPerSubjectPerHour
	}
	if o.MaxFields != nil {
		out.MaxFields = o.MaxFields
	}
	return &out
}

// validateTaskBounds checks one scope's task bounds name event globs and
// known kinds, and set positive caps.
func validateTaskBounds(where string, b *TaskBounds) error {
	if b == nil {
		return nil
	}
	for i, g := range b.Events {
		if strings.TrimSpace(g) == "" || !doublestar.ValidatePattern(g) {
			return fmt.Errorf("configfile: %s.events[%d] %q is not a valid glob", where, i, g)
		}
	}
	for _, c := range []struct {
		key   string
		got   []string
		known []string
	}{{"actions", b.Actions, taskActionKinds}, {"context", b.Context, taskContextKinds}} {
		for i, k := range c.got {
			if !slices.Contains(c.known, k) {
				return fmt.Errorf("configfile: %s.%s[%d] must be one of %s, got %q", where, c.key, i, strings.Join(c.known, ", "), k)
			}
		}
	}
	for i, tool := range b.Tools {
		if !toolRe.MatchString(tool) {
			return fmt.Errorf("configfile: %s.tools[%d] %q is not a tool name", where, i, tool)
		}
	}
	for _, c := range []struct {
		key string
		v   *int
	}{{"maxTasks", b.MaxTasks}, {"maxRunsPerSubjectPerHour", b.MaxRunsPerSubjectPerHour}, {"maxFields", b.MaxFields}} {
		if c.v != nil && *c.v <= 0 {
			return fmt.Errorf("configfile: %s.%s must be positive", where, c.key)
		}
	}
	return nil
}

// toolRe is an agent tool's name.
var toolRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// scopedTask is an operator task and whether the operator's file, rather
// than the dashboard, wrote it.
type scopedTask struct {
	task tasks.Task
	file bool
}

// mergeTasks lays the tasks one scope writes over ts by name: one with a
// name ts has replaces it where it stands, any other is added.
func mergeTasks(ts []scopedTask, scope []tasks.Task, file bool) []scopedTask {
	for _, t := range scope {
		if i := slices.IndexFunc(ts, func(x scopedTask) bool { return x.task.Name == t.Name }); i >= 0 {
			ts[i] = scopedTask{t, file}
		} else {
			ts = append(ts, scopedTask{t, file})
		}
	}
	return ts
}

// setTasks splits the merged tasks into the file's and the dashboard's,
// leaving out those switched off.
func (s *Settings) setTasks(ts []scopedTask) {
	for _, t := range ts {
		switch {
		case !t.task.IsEnabled():
		case t.file:
			s.Tasks = append(s.Tasks, t.task)
		default:
			s.DashboardTasks = append(s.DashboardTasks, t.task)
		}
	}
}

// validateTasks checks each task one scope writes, that its models name
// declared providers, and that no two share a name.
func (f *File) validateTasks(where string, ts []tasks.Task) error {
	for i := range ts {
		if err := ts[i].Check(); err != nil {
			return fmt.Errorf("configfile: %s.tasks[%d]: %w", where, i, err)
		}
		models := ModelsSpec{Review: new(ModelRef(ts[i].Models.Review)), Fallback: new(ModelRef(ts[i].Models.Fallback))}
		if err := f.checkModels(fmt.Sprintf("%s.tasks[%d].models", where, i), models); err != nil {
			return err
		}
		if slices.ContainsFunc(ts[:i], func(o tasks.Task) bool { return o.Name == ts[i].Name }) {
			return fmt.Errorf("configfile: %s.tasks[%d]: name %q is used twice", where, i, ts[i].Name)
		}
	}
	return nil
}
