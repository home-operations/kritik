package taskrun

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/tasks"
)

// Action names a dropped write carries, matching tasks.Plan's.
const (
	dropAddLabel    = "labels.add"
	dropRemoveLabel = "labels.remove"
	dropInline      = "inline"
	dropComment     = "comment"
)

// Anchor keeps the inline comments whose every line the pull request's diff
// shows, as anchors (see review.Anchors) gives them: the forge refuses a
// comment on any other line.
func Anchor(inline []tasks.Inline, anchors map[string]map[int]bool) (kept []tasks.Inline, dropped []tasks.Drop) {
	for _, c := range inline {
		end := max(c.EndLine, c.Line)
		ok := c.Line > 0
		for l := c.Line; ok && l <= end; l++ {
			ok = anchors[c.Path][l]
		}
		if ok {
			kept = append(kept, c)
			continue
		}
		dropped = append(dropped, tasks.Drop{Action: dropInline, Value: inlineValue(c), Reason: "the line is not in the pull request's diff"})
	}
	return kept, dropped
}

// DropInline drops every inline comment of pl for reason.
func DropInline(pl *tasks.Plan, reason string) {
	for _, c := range pl.Inline {
		pl.Dropped = append(pl.Dropped, tasks.Drop{Action: dropInline, Value: inlineValue(c), Reason: reason})
	}
	pl.Inline = nil
}

func inlineValue(c tasks.Inline) string {
	if c.EndLine > c.Line {
		return fmt.Sprintf("%s:%d-%d", c.Path, c.Line, c.EndLine)
	}
	return c.Path + ":" + strconv.Itoa(c.Line)
}

// Target is the issue or pull request a plan writes to.
type Target struct {
	Owner, Repo string
	Number      int
	// HeadSHA pins inline comments to a pull request's head.
	HeadSHA string
}

// Result is what applying a plan's writes did.
type Result struct {
	// Applied are the writes the forge accepted.
	Applied tasks.Applied
	// Dropped are the writes the forge refused, each with its error.
	Dropped []tasks.Drop
	// Attempted counts the forge calls made.
	Attempted int
}

// Failed reports whether every write attempted failed.
func (r Result) Failed() bool { return r.Attempted > 0 && len(r.Dropped) == r.Attempted }

// Apply makes pl's writes other than its comment, in order: labels added
// and removed, assignees, reviewers, state, then inline comments. A write
// the forge refuses is recorded and the rest still made, since each is
// independent of the others.
func Apply(ctx context.Context, c forge.Client, t Target, pl tasks.Plan) Result {
	var r Result
	try := func(action, value string, write func() error) bool {
		r.Attempted++
		if err := write(); err != nil {
			r.Dropped = append(r.Dropped, tasks.Drop{Action: action, Value: value, Reason: "forge: " + err.Error()})
			return false
		}
		return true
	}
	if len(pl.AddLabels) > 0 && try(dropAddLabel, strings.Join(pl.AddLabels, ", "), func() error {
		return c.AddLabels(ctx, t.Owner, t.Repo, t.Number, pl.AddLabels)
	}) {
		r.Applied.AddLabels = pl.AddLabels
	}
	for _, l := range pl.RemoveLabels {
		if try(dropRemoveLabel, l, func() error { return c.RemoveLabel(ctx, t.Owner, t.Repo, t.Number, l) }) {
			r.Applied.RemoveLabels = append(r.Applied.RemoveLabels, l)
		}
	}
	if len(pl.Assignees) > 0 && try(tasks.ActionAssign, strings.Join(pl.Assignees, ", "), func() error {
		return c.AddAssignees(ctx, t.Owner, t.Repo, t.Number, pl.Assignees)
	}) {
		r.Applied.Assignees = pl.Assignees
	}
	if len(pl.Reviewers) > 0 && try(tasks.ActionReviewers, strings.Join(pl.Reviewers, ", "), func() error {
		return c.RequestReviewers(ctx, t.Owner, t.Repo, t.Number, pl.Reviewers)
	}) {
		r.Applied.Reviewers = pl.Reviewers
	}
	if pl.State != "" && try(tasks.ActionState, pl.State, func() error {
		return c.SetState(ctx, t.Owner, t.Repo, t.Number, pl.State == tasks.StateReopen)
	}) {
		r.Applied.State = pl.State
	}
	if len(pl.Inline) > 0 && try(dropInline, strconv.Itoa(len(pl.Inline))+" comment(s)", func() error {
		return c.CreateReview(ctx, t.Owner, t.Repo, t.Number, t.HeadSHA, inlineComments(pl.Inline, c.LineRanges()))
	}) {
		r.Applied.Inline = pl.Inline
	}
	return r
}

func inlineComments(in []tasks.Inline, ranges bool) []forge.InlineComment {
	out := make([]forge.InlineComment, len(in))
	for i, c := range in {
		out[i] = forge.InlineComment{Path: c.Path, Line: c.Line, Body: c.Body}
		if c.EndLine > c.Line && ranges {
			out[i].StartLine, out[i].Line = c.Line, c.EndLine
		}
	}
	return out
}

// Post posts a task's report comment: in place of its sticky comment, the
// one by login carrying the task's marker, when mode is sticky and there is
// one, and as a new comment otherwise. It returns the comment's id.
func Post(ctx context.Context, c forge.Client, t Target, task, login, mode, body string) (int64, error) {
	if mode != tasks.CommentSticky {
		return c.CreateComment(ctx, t.Owner, t.Repo, t.Number, body)
	}
	marker := tasks.StickyMarker(task)
	body += "\n\n" + marker
	id, err := c.FindComment(ctx, t.Owner, t.Repo, t.Number, login, marker)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return c.CreateComment(ctx, t.Owner, t.Repo, t.Number, body)
	}
	return id, c.UpdateComment(ctx, t.Owner, t.Repo, id, body)
}

// CommentDrop records a report comment the forge refused.
func CommentDrop(mode string, err error) tasks.Drop {
	return tasks.Drop{Action: dropComment, Value: mode, Reason: "forge: " + err.Error()}
}
