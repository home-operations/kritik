package repoconfig

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/tasks"
)

// mergeTasks sets m.Tasks to the tasks that run: the operator's file's,
// trusted whole, then the dashboard's, then the file's, the last two
// clipped to the operator's task bounds and held to its mode, model, agent
// and command bounds, with a note in m.TaskNotes for everything left out.
// A tenant admin writes the dashboard's, so they get no more than a
// repository does, bar the repositoryTasks switch. A file's task named like
// an operator's is dropped.
func (m *Merged) mergeTasks(f *File, op *configfile.Settings) {
	b := op.TaskBounds
	m.Tasks, m.TaskNotes = tasks.Clip(op.Tasks, b, false)
	dash := b
	dash.RepositoryTasks = true
	m.bounded(op.DashboardTasks, dash, op)
	m.DashboardTasks = nil
	var own []tasks.Task
	for _, t := range f.Tasks {
		if slices.ContainsFunc(append(slices.Clone(op.Tasks), op.DashboardTasks...), func(o tasks.Task) bool { return o.Name == t.Name }) {
			m.TaskNotes = append(m.TaskNotes, tasks.Note{Task: t.Name, What: "task", Reason: "an operator task has the same name"})
			continue
		}
		own = append(own, t)
	}
	m.bounded(own, b, op)
}

// bounded adds the tasks of ts that b and op's bounds let run.
func (m *Merged) bounded(ts []tasks.Task, b tasks.Bounds, op *configfile.Settings) {
	kept, notes := tasks.Clip(ts, b, true)
	m.TaskNotes = append(m.TaskNotes, notes...)
	for _, t := range kept {
		notes, ok := chooseTask(&t, op)
		m.TaskNotes = append(m.TaskNotes, notes...)
		if ok {
			m.Tasks = append(m.Tasks, t)
		}
	}
}

// chooseTask clears each model, agent limit, command list and context
// command t chooses outside the bound op.Allow gives a repository, so the
// operator's applies, and notes it. A task whose mode is outside the
// bound is left out: ok is false.
func chooseTask(t *tasks.Task, op *configfile.Settings) (notes []tasks.Note, ok bool) {
	note := func(what, value, allowed string) {
		notes = append(notes, tasks.Note{Task: t.Name, What: what, Reason: fmt.Sprintf("%s was dropped; allowed: %s", value, allowed)})
	}
	a := op.Allow
	modes := a.Modes
	if modes == nil {
		modes = []configfile.ReviewMode{op.Mode}
	}
	if mode := configfile.ReviewMode(t.RunMode()); !slices.Contains(modes, mode) {
		note("mode", strconv.Quote(string(mode)), list(modes))
		return notes, false
	}
	commands := a.Commands
	if commands == nil {
		commands = op.Agent.Commands
	}
	if cs := t.Context.Commands; cs != nil {
		t.Context.Commands = nil
		for _, c := range cs {
			if slices.Contains(commands, c.Binary()) {
				t.Context.Commands = append(t.Context.Commands, c)
			} else {
				note("context.commands."+c.Name, strconv.Quote(c.Binary()), list(commands))
			}
		}
	}
	for _, c := range []struct {
		what string
		dst  *string
		own  configfile.ModelRef
	}{{"models.review", &t.Models.Review, op.Models.Review}, {"models.fallback", &t.Models.Fallback, op.Models.Fallback}} {
		if *c.dst == "" {
			continue
		}
		models := a.Models
		if models == nil && c.own != "" {
			models = []configfile.ModelRef{c.own}
		}
		if !slices.Contains(models, configfile.ModelRef(*c.dst)) {
			note(c.what, strconv.Quote(*c.dst), list(models))
			*c.dst = ""
		}
	}
	if t.Agent.Commands != nil {
		if i := slices.IndexFunc(t.Agent.Commands, func(c string) bool { return !slices.Contains(commands, c) }); i >= 0 {
			note("agent.commands", strconv.Quote(t.Agent.Commands[i]), list(commands))
			t.Agent.Commands = nil
		}
	}
	t.Agent.MaxSteps = atMost(note, "agent.maxSteps", t.Agent.MaxSteps, a.Agent.MaxSteps, op.Agent.MaxSteps)
	t.Agent.MaxToolOutputBytes = atMost(note, "agent.maxToolOutputBytes", t.Agent.MaxToolOutputBytes, a.Agent.MaxToolOutputBytes,
		op.Agent.MaxToolOutputBytes)
	t.Agent.MaxTokens = atMost(note, "agent.maxTokens", t.Agent.MaxTokens, a.Agent.MaxTokens, op.Agent.MaxTokens)
	t.Agent.Timeout = atMost(note, "agent.timeout", t.Agent.Timeout, a.Agent.Timeout, op.Agent.Timeout)
	return notes, true
}

// atMost is want when it is at most bound, or when bound is nil at most
// own; otherwise nil, noted.
func atMost[T int | int64 | time.Duration](note func(what, value, allowed string), what string, want, bound *T, own T) *T {
	if want == nil {
		return nil
	}
	limit := own
	if bound != nil {
		limit = *bound
	}
	if *want > limit {
		note(what, fmt.Sprint(*want), fmt.Sprintf("at most %v", limit))
		return nil
	}
	return want
}
