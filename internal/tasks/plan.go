package tasks

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// loginRe is a forge login: what a user rule may render and a proposal
// may name.
var loginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Facts are what Plan checks an answer against.
type Facts struct {
	// RepoLabels are every label the repository has.
	RepoLabels []string
	// Subject is the issue or pull request as it is now; nil for an event
	// about none.
	Subject *Subject
	// UserAllowed reports whether a login may be assigned or asked for a
	// review, such as whether it has at least read access; nil allows
	// none.
	UserAllowed func(login string) bool
}

// CommentAction is the report comment to post.
type CommentAction struct {
	// Mode is CommentSticky or CommentAppend.
	Mode string
	Body string
}

// Drop is a proposed or ruled action Plan left out, and why.
type Drop struct {
	Action string
	Value  string
	Reason string
}

// Plan is the validated writes an answer leads to.
type Plan struct {
	Comment      *CommentAction
	AddLabels    []string
	RemoveLabels []string
	Assignees    []string
	Reviewers    []string
	// State is StateClose, StateReopen or "" for no change.
	State   string
	Inline  []Inline
	Dropped []Drop
}

// Applied is the plan's actions, for templates.
func (pl *Plan) Applied() Applied {
	return Applied{
		AddLabels: pl.AddLabels, RemoveLabels: pl.RemoveLabels, State: pl.State,
		Assignees: pl.Assignees, Reviewers: pl.Reviewers, Inline: pl.Inline,
	}
}

// Dropped action names.
const (
	dropAddLabel    = "labels.add"
	dropRemoveLabel = "labels.remove"
	dropComment     = "comment"
	dropInline      = "inline"
)

// Plan turns a validated answer into the writes to make: what the model
// proposed within the task's allowlists, together with what the task's
// rules add when their ifs hold, each checked against f. A label must
// exist in the repository, and a proposed one must match a propose glob;
// a user must be well formed, allowed by f.UserAllowed and, when
// proposed, listed; a state change must be declared; inline comments and
// review requests need a pull request. A block whose if is false, or
// fails, contributes nothing. When a label is both added and removed, the
// add wins. Everything left out is in Dropped with its reason. Finally the
// report comment is rendered with the planned actions as .Applied.
func (p *Prepared) Plan(in Input, a Answer, f Facts) (Plan, error) {
	pb := planner{p: p, in: in, a: a, f: f, vars: vars(in, &a)}
	if f.Subject != nil {
		pb.in.Subject = f.Subject
		pb.vars["subject"] = subjectVars(f.Subject)
	}
	pb.labels()
	pb.state()
	pb.assign()
	pb.reviewers()
	pb.inline()
	pb.comment()
	return pb.plan, nil
}

type planner struct {
	p    *Prepared
	in   Input
	a    Answer
	f    Facts
	vars map[string]any
	plan Plan
}

func (pb *planner) drop(action, value, reason string) {
	pb.plan.Dropped = append(pb.plan.Dropped, Drop{Action: action, Value: value, Reason: reason})
}

// blocked says why g does not hold, "" when it does. A guard that fails
// is noted under action, since no value may carry the reason.
func (pb *planner) blocked(g *guard, action string) string {
	ok, err := g.eval(pb.vars)
	switch {
	case err != nil:
		reason := "its if failed: " + err.Error()
		pb.drop(action, g.src, reason)
		return reason
	case !ok:
		return "its if is false"
	}
	return ""
}

// holds evaluates g, dropping values under action when it does not hold.
func (pb *planner) holds(g *guard, action string, values []string) bool {
	reason := pb.blocked(g, action)
	if reason == "" {
		return true
	}
	pb.dropAll(action, values, reason)
	return false
}

// output is the data a rule template renders.
func (pb *planner) output() OutputData {
	return OutputData{Input: pb.in, Task: pb.p.Task, Answer: pb.a}
}

// subjectless drops values under action when there is no subject.
func (pb *planner) subjectless(action string, values []string) bool {
	if pb.in.Subject != nil {
		return false
	}
	for _, v := range values {
		pb.drop(action, v, "the event has no issue or pull request")
	}
	return true
}

func (pb *planner) labels() {
	spec := pb.p.Task.Actions.Labels
	if spec == nil {
		pb.dropAll(dropAddLabel, pb.a.Labels.Add, "the task declares no labels action")
		pb.dropAll(dropRemoveLabel, pb.a.Labels.Remove, "the task declares no labels action")
		return
	}
	proposed := append(slices.Clone(pb.a.Labels.Add), pb.a.Labels.Remove...)
	if pb.subjectless(ActionLabels, proposed) {
		return
	}
	var ruleAdd, ruleRemove []string
	for i, r := range pb.p.labelRules {
		if pb.holds(pb.p.guards.labelRules[i], ActionLabels, nil) {
			ruleAdd = append(ruleAdd, pb.ruleValues(dropAddLabel, i, r.add)...)
			ruleRemove = append(ruleRemove, pb.ruleValues(dropRemoveLabel, i, r.remove)...)
		}
	}
	if reason := pb.blocked(pb.p.guards.labels, ActionLabels); reason != "" {
		pb.dropAll(dropAddLabel, dedupe(slices.Concat(pb.a.Labels.Add, ruleAdd)), reason)
		pb.dropAll(dropRemoveLabel, dedupe(slices.Concat(pb.a.Labels.Remove, ruleRemove)), reason)
		return
	}
	var add, remove []string
	for _, l := range pb.a.Labels.Add {
		if !matchAny(spec.Propose.Add, l) {
			pb.drop(dropAddLabel, l, "it is not a label the task lets the model add")
			continue
		}
		add = append(add, l)
	}
	for _, l := range pb.a.Labels.Remove {
		if !matchAny(spec.Propose.Remove, l) {
			pb.drop(dropRemoveLabel, l, "it is not a label the task lets the model remove")
			continue
		}
		remove = append(remove, l)
	}
	add, remove = dedupe(append(add, ruleAdd...)), dedupe(append(remove, ruleRemove...))
	for _, l := range add {
		if pb.checkLabel(dropAddLabel, l) {
			pb.plan.AddLabels = append(pb.plan.AddLabels, l)
		}
	}
	for _, l := range remove {
		switch {
		case slices.Contains(pb.plan.AddLabels, l):
			pb.drop(dropRemoveLabel, l, "it is also added, and an add wins")
		case pb.checkLabel(dropRemoveLabel, l):
			pb.plan.RemoveLabels = append(pb.plan.RemoveLabels, l)
		}
	}
}

// ruleValues renders rule i's templates, dropping the rule under action
// when one fails.
func (pb *planner) ruleValues(action string, i int, ts []*template.Template) []string {
	vs, err := renderValues(ts, pb.output())
	if err != nil {
		pb.drop(action, fmt.Sprintf("rules[%d]", i), "its template failed: "+err.Error())
		return nil
	}
	return vs
}

func (pb *planner) dropAll(action string, values []string, reason string) {
	for _, v := range values {
		pb.drop(action, v, reason)
	}
}

func (pb *planner) checkLabel(action, l string) bool {
	if !slices.Contains(pb.f.RepoLabels, l) {
		pb.drop(action, l, "the repository has no such label")
		return false
	}
	return true
}

func (pb *planner) state() {
	spec := pb.p.Task.Actions.State
	var proposed []string
	if pb.a.State != "" {
		proposed = []string{pb.a.State}
	}
	if spec == nil {
		pb.dropAll(ActionState, proposed, "the task declares no state action")
		return
	}
	if pb.subjectless(ActionState, proposed) {
		return
	}
	var ruled []string
	for i, r := range spec.Rules {
		if pb.holds(pb.p.guards.stateRules[i], ActionState, nil) {
			s := StateClose
			if r.Reopen {
				s = StateReopen
			}
			ruled = append(ruled, s)
		}
	}
	if !pb.holds(pb.p.guards.state, ActionState, dedupe(slices.Concat(proposed, ruled))) {
		return
	}
	want := ""
	switch pb.a.State {
	case StateClose, StateReopen:
		if (pb.a.State == StateClose && spec.Propose.Close) || (pb.a.State == StateReopen && spec.Propose.Reopen) {
			want = pb.a.State
		} else {
			pb.drop(ActionState, pb.a.State, "the task does not let the model "+pb.a.State+" the subject")
		}
	}
	for _, r := range ruled {
		if want != "" && want != r {
			pb.drop(ActionState, want, "a rule changes the state to "+r)
		}
		want = r
	}
	switch {
	case want == "":
	case (want == StateClose && pb.in.Subject.State == "closed") || (want == StateReopen && pb.in.Subject.State == "open"):
		pb.drop(ActionState, want, "the subject is already "+pb.in.Subject.State)
	default:
		pb.plan.State = want
	}
}

func (pb *planner) assign() {
	g := pb.p.guards
	pb.plan.Assignees = pb.users(ActionAssign, pb.p.Task.Actions.Assign, pb.a.Assignees, g.assign, pb.p.assignRules, g.assignRules)
}

func (pb *planner) reviewers() {
	spec := pb.p.Task.Actions.Reviewers
	if spec != nil && pb.in.Subject != nil && pb.in.Subject.Kind != SubjectPull {
		pb.dropAll(ActionReviewers, pb.a.Reviewers, "reviews can only be requested on a pull request")
		return
	}
	got := pb.users(ActionReviewers, spec, pb.a.Reviewers, pb.p.guards.reviewers, pb.p.reviewerRules, pb.p.guards.reviewerRules)
	for _, u := range got {
		if pb.in.Subject != nil && strings.EqualFold(u, pb.in.Subject.Author) {
			pb.drop(ActionReviewers, u, "the author cannot review their own pull request")
			continue
		}
		pb.plan.Reviewers = append(pb.plan.Reviewers, u)
	}
}

// users plans one users action: proposals from the allowlist, and what the
// rules render, each well formed and allowed by the facts.
func (pb *planner) users(action string, spec *UsersSpec, proposed []string, g *guard, rules []ruleTemplates, ruleGuards []*guard) []string {
	if spec == nil {
		pb.dropAll(action, proposed, "the task declares no "+action+" action")
		return nil
	}
	if pb.subjectless(action, proposed) {
		return nil
	}
	var ruled []string
	for i, r := range rules {
		if pb.holds(ruleGuards[i], action, nil) {
			ruled = append(ruled, pb.ruleValues(action, i, r.add)...)
		}
	}
	if !pb.holds(g, action, dedupe(slices.Concat(proposed, ruled))) {
		return nil
	}
	var want []string
	for _, u := range proposed {
		if !slices.Contains(spec.Propose.Users, u) {
			pb.drop(action, u, "it is not a user the task lets the model pick")
			continue
		}
		want = append(want, u)
	}
	want = append(want, ruled...)
	var out []string
	for _, u := range dedupe(want) {
		switch {
		case !loginRe.MatchString(u):
			pb.drop(action, u, "it is not a valid login")
		case pb.f.UserAllowed == nil || !pb.f.UserAllowed(u):
			pb.drop(action, u, "the user is not allowed")
		default:
			out = append(out, u)
		}
	}
	return out
}

func (pb *planner) inline() {
	values := make([]string, len(pb.a.Inline))
	for i, c := range pb.a.Inline {
		values[i] = fmt.Sprintf("%s:%d", c.Path, c.Line)
	}
	spec := pb.p.Task.Actions.InlineComments
	switch {
	case len(values) == 0:
		return
	case spec == nil || !spec.Propose:
		pb.dropAll(dropInline, values, "the task declares no inline comments")
		return
	case pb.in.Subject == nil || pb.in.Subject.Kind != SubjectPull:
		pb.dropAll(dropInline, values, "inline comments need a pull request")
		return
	case !pb.holds(pb.p.guards.inline, dropInline, values):
		return
	}
	seen := map[Inline]bool{}
	for _, c := range pb.a.Inline {
		if !seen[c] {
			seen[c] = true
			pb.plan.Inline = append(pb.plan.Inline, c)
		}
	}
}

func (pb *planner) comment() {
	spec := pb.p.Task.Actions.Comment
	if spec == nil || spec.PostMode() == CommentNone || pb.subjectless(dropComment, []string{spec.PostMode()}) {
		return
	}
	if !pb.holds(pb.p.guards.comment, dropComment, []string{spec.PostMode()}) {
		return
	}
	out := pb.output()
	out.Applied, out.Dropped = pb.plan.Applied(), pb.plan.Dropped
	body, err := pb.p.RenderComment(out)
	if err != nil {
		pb.drop(dropComment, spec.PostMode(), "its template failed: "+err.Error())
		return
	}
	if body != "" {
		pb.plan.Comment = &CommentAction{Mode: spec.PostMode(), Body: body}
	}
}

// dedupe keeps the first of each value, in order.
func dedupe(vs []string) []string {
	var out []string
	for _, v := range vs {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
