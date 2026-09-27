// Package taskrun adapts webhook deliveries and forge objects to the pure
// tasks package, and holds the decisions around running a task that need
// no I/O: whether a delivery can match any task, which proposed inline
// comments a pull request's diff anchors, and the order a plan's writes
// are applied in.
package taskrun

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/tasks"
	"github.com/home-operations/kritik/internal/webhook"
)

// MaxRawBytes caps the payload a task sees decoded; a larger one is seen as
// empty. The webhook body limit bounds what is stored.
const MaxRawBytes = 1 << 20

// Event is the normalized event a webhook kind is, "" for a kind with none.
func Event(k webhook.Kind) string {
	switch k {
	case webhook.KindIssue:
		return tasks.EventIssue
	case webhook.KindPullRequest:
		return tasks.EventPullRequest
	case webhook.KindComment:
		return tasks.EventComment
	}
	return ""
}

// DecodeRaw decodes a JSON object payload, nil when it is not one or is
// over MaxRawBytes.
func DecodeRaw(raw []byte) map[string]any {
	if len(raw) == 0 || len(raw) > MaxRawBytes {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// Input is ev as a task sees it, with the subject the delivery itself
// describes.
func Input(ev webhook.Event) tasks.Input {
	in := tasks.Input{
		Forge: string(ev.Forge), Event: Event(ev.Kind), RawEvent: ev.RawEvent, Action: ev.Action, Sender: ev.Sender,
		Subject: eventSubject(ev), Raw: DecodeRaw(ev.Raw),
	}
	if r := ev.Repository; r != nil {
		in.Repo = Repo(r.FullName, r.DefaultBranch)
	}
	return in
}

// Repo is the repository fullName ("owner/name") names.
func Repo(fullName, defaultBranch string) tasks.Repo {
	owner, name, _ := strings.Cut(fullName, "/")
	return tasks.Repo{Owner: owner, Name: name, DefaultBranch: defaultBranch}
}

func eventSubject(ev webhook.Event) *tasks.Subject {
	switch {
	case ev.Issue != nil:
		i := ev.Issue
		return &tasks.Subject{
			Kind: tasks.SubjectIssue, Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, Author: i.Author, URL: i.URL,
			Labels: slices.Clone(i.Labels), Assignees: slices.Clone(i.Assignees),
		}
	case ev.PullRequest != nil:
		p := ev.PullRequest
		labels := make([]string, len(p.Labels))
		for i, l := range p.Labels {
			labels[i] = l.Name
		}
		return &tasks.Subject{
			Kind: tasks.SubjectPull, Number: p.Number, Title: p.Title, Body: p.Body, State: p.State, Author: p.Author, URL: p.URL,
			Labels: labels, Draft: p.Draft,
		}
	case ev.Subject != nil:
		return &tasks.Subject{Kind: ev.Subject.Kind, Number: ev.Subject.Number}
	}
	return nil
}

// Subject is an issue or pull request as the forge holds it now.
func Subject(i forge.Issue) *tasks.Subject {
	kind := tasks.SubjectIssue
	if i.IsPull {
		kind = tasks.SubjectPull
	}
	return &tasks.Subject{
		Kind: kind, Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, Author: i.Author, URL: i.URL,
		Labels: slices.Clone(i.Labels), Assignees: slices.Clone(i.Assignees),
	}
}

// Names are the event names in fires, as triggers and the operator's
// bounds glob them: the normalized one, when it has one, and the raw one.
func Names(in tasks.Input) []string {
	var out []string
	if in.Event != "" {
		out = append(out, in.Event+"."+in.Action)
	}
	if in.RawEvent != "" {
		out = append(out, "raw:"+in.RawEvent+"."+in.Action)
	}
	return out
}

// Candidate reports whether in could run any task: a trigger of one of
// operator, the operator's tasks, names it, or, when b lets a repository
// define tasks, one of the events b lets them trigger on does. It ignores
// guards and cannot see the repository's own tasks, so it may say yes for
// an event no task runs on, but never no for one a task does.
func Candidate(in tasks.Input, operator []tasks.Task, b tasks.Bounds) bool {
	if !b.Enabled {
		return false
	}
	names := Names(in)
	matches := func(globs []string) bool {
		for _, g := range globs {
			for _, n := range names {
				if ok, _ := doublestar.Match(g, n); ok {
					return true
				}
			}
		}
		return false
	}
	for _, t := range operator {
		for _, tr := range t.On {
			if matches(tr.Names()) {
				return true
			}
		}
	}
	return b.RepositoryTasks && matches(b.Events)
}
