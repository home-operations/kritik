package tasks

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/home-operations/kritik/internal/jobtimeout"
)

var (
	nameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	fieldNameRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*$`)
	// commandRe is a binary name, as the operator's allowed commands are.
	commandRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	toolRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// shellChars are refused in a context command: it runs without a shell,
// and a line that looks like shell would not do what it says.
const shellChars = "|;&$`<>(){}\\\"'*?~\n"

// Definition bounds.
const (
	maxThreadComments = 200
	maxContextK       = 50
	maxContextFiles   = 50
)

// subjectEvents are the raw event names whose deliveries always carry an
// issue or pull request.
var subjectEvents = []string{rawIssues, "issue_comment", "pull_request", "pull_request_review_comment", "pull_request_comment"}

// issueOnlyEvents are the raw event names that never carry a pull request.
var issueOnlyEvents = []string{rawIssues}

// rawIssues is the issue event's name on every forge.
const rawIssues = "issues"

// Check validates t on its own: its name, triggers, guards, fields,
// context, templates and paths, and that its actions fit its triggers.
// Actions that write to an issue or pull request are refused on a task a
// subject-less event can trigger, and inline comments on one only issues
// can. A task with enabled false only switches off a task of the same
// name, so only its name is checked. Errors name the offending key; the
// caller adds where the task is.
func (t *Task) Check() error {
	if !nameRe.MatchString(t.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and dashes, at most 63, starting with a letter or digit", t.Name)
	}
	if !t.IsEnabled() {
		return nil
	}
	if len(t.On) == 0 {
		return errors.New("on needs at least one trigger")
	}
	for i, tr := range t.On {
		if err := tr.check(); err != nil {
			return fmt.Errorf("on[%d]: %w", i, err)
		}
	}
	if _, err := compileGuard(t.If, false); err != nil {
		if _, aerr := compileGuard(t.If, true); aerr == nil {
			return errors.New("if cannot use answer; only an action's if sees the model's answer")
		}
		return err
	}
	switch t.Mode {
	case "", ModeAgentic, ModeSingle:
	default:
		return fmt.Errorf("mode must be %s or %s, got %q", ModeAgentic, ModeSingle, t.Mode)
	}
	for _, m := range []struct{ role, ref string }{{"review", t.Models.Review}, {"fallback", t.Models.Fallback}} {
		if p, id, ok := strings.Cut(m.ref, "/"); m.ref != "" && (!ok || p == "" || id == "") {
			return fmt.Errorf("models.%s must be \"<provider>/<model>\", got %q", m.role, m.ref)
		}
	}
	if err := t.Agent.check(); err != nil {
		return err
	}
	if err := t.checkContext(); err != nil {
		return err
	}
	if t.Prompt != "" && t.PromptInline != "" {
		return errors.New("set prompt or promptInline, not both")
	}
	for _, p := range []struct{ key, path string }{{"system", t.System}, {"prompt", t.Prompt}} {
		if p.path != "" {
			if err := checkRefPath(p.path); err != nil {
				return fmt.Errorf("%s: %w", p.key, err)
			}
		}
	}
	if t.PromptInline != "" {
		if _, err := parseTemplate("promptInline", t.PromptInline); err != nil {
			return err
		}
	}
	if err := checkFields(keyFields, t.Fields, true); err != nil {
		return err
	}
	return t.checkActions()
}

func (tr Trigger) check() error {
	switch tr.Event {
	case EventIssue, EventPullRequest, EventComment:
		if tr.RawEvent != "" {
			return fmt.Errorf("only a raw trigger names an event")
		}
	case EventRaw:
		if !validGlob(tr.RawEvent) {
			return fmt.Errorf("raw.event %q is not a valid glob", tr.RawEvent)
		}
	default:
		return fmt.Errorf("unknown trigger %q", tr.Event)
	}
	for i, a := range tr.Actions {
		if !validGlob(a) {
			return fmt.Errorf("actions[%d] %q is not a valid glob", i, a)
		}
	}
	return nil
}

// subjectless reports whether a delivery tr matches may carry no issue or
// pull request.
func (tr Trigger) subjectless() bool {
	return tr.Event == EventRaw && !slices.Contains(subjectEvents, tr.RawEvent)
}

// pullCapable reports whether a delivery tr matches may be about a pull
// request.
func (tr Trigger) pullCapable() bool {
	return tr.Event != EventIssue && (tr.Event != EventRaw || !slices.Contains(issueOnlyEvents, tr.RawEvent))
}

func (a Agent) check() error {
	if (a.MaxSteps != nil && *a.MaxSteps <= 0) || (a.MaxToolOutputBytes != nil && *a.MaxToolOutputBytes <= 0) ||
		(a.MaxTokens != nil && *a.MaxTokens <= 0) || (a.Timeout != nil && *a.Timeout <= 0) {
		return errors.New("agent limits must be positive")
	}
	if a.Timeout != nil && *a.Timeout > jobtimeout.MaxAgentTimeout {
		return fmt.Errorf("agent.timeout must not exceed %s", jobtimeout.MaxAgentTimeout)
	}
	for i, c := range a.Commands {
		if !commandRe.MatchString(c) {
			return fmt.Errorf("agent.commands[%d] %q must be a bare command name, not a path", i, c)
		}
	}
	for i, tool := range a.Tools {
		if !toolRe.MatchString(tool) {
			return fmt.Errorf("agent.tools[%d] %q is not a tool name", i, tool)
		}
	}
	return nil
}

func (t *Task) checkContext() error {
	c := t.Context
	if c.Thread != nil && (c.Thread.Comments < 0 || c.Thread.Comments > maxThreadComments) {
		return fmt.Errorf("context.thread.comments must be 0 to %d", maxThreadComments)
	}
	for i, f := range c.Files {
		where := fmt.Sprintf("context.files[%d]", i)
		switch {
		case (f.Path == "") == (f.Glob == ""):
			return fmt.Errorf("%s: set path or glob, not both", where)
		case f.Path != "":
			if err := checkRefPath(f.Path); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			if f.Max != 0 {
				return fmt.Errorf("%s: max applies to a glob", where)
			}
		default:
			if !validGlob(f.Glob) || checkRefPath(f.Glob) != nil {
				return fmt.Errorf("%s: glob %q is not a valid glob inside the repository", where, f.Glob)
			}
			if f.Max < 0 || f.Max > maxContextFiles {
				return fmt.Errorf("%s: max must be 0 to %d", where, maxContextFiles)
			}
		}
	}
	// .Context.files holds the files, and the runner reports what it left
	// out as notes.
	names := []string{ContextFiles, "notes"}
	for _, q := range []struct {
		kind string
		qs   []Query
	}{{ContextSearch, c.Search}, {ContextRelated, c.Related}} {
		for i, x := range q.qs {
			where := fmt.Sprintf("context.%s[%d]", q.kind, i)
			if err := checkName(where, x.Name, &names); err != nil {
				return err
			}
			if strings.TrimSpace(x.Query) == "" {
				return fmt.Errorf("%s: query is required", where)
			}
			if _, err := parseTemplate(where+".query", x.Query); err != nil {
				return err
			}
			if x.K < 0 || x.K > maxContextK {
				return fmt.Errorf("%s: k must be 0 to %d", where, maxContextK)
			}
		}
	}
	if len(c.Commands) > 0 && t.RunMode() != ModeAgentic {
		return errors.New("context.commands need mode agentic")
	}
	for i, x := range c.Commands {
		where := fmt.Sprintf("context.commands[%d]", i)
		if err := checkName(where, x.Name, &names); err != nil {
			return err
		}
		if !commandRe.MatchString(x.Binary()) {
			return fmt.Errorf("%s: run must start with a bare command name, not a path", where)
		}
		if strings.ContainsAny(x.Run, shellChars) {
			return fmt.Errorf("%s: run is not given to a shell, so it may not hold any of %q", where, shellChars)
		}
	}
	return nil
}

// checkName checks a context source's name is a template-friendly
// identifier no other source in names has, and adds it.
func checkName(where, name string, names *[]string) error {
	if !fieldNameRe.MatchString(name) {
		return fmt.Errorf("%s: name %q must start with a lowercase letter and hold only letters, digits and _", where, name)
	}
	if slices.Contains(*names, name) {
		return fmt.Errorf("%s: name %q is used twice or reserved", where, name)
	}
	*names = append(*names, name)
	return nil
}

// checkFields validates field declarations; nested is false inside an
// array or object, which hold scalars only.
func checkFields(where string, fs Fields, nested bool) error {
	var seen []string
	for _, f := range fs {
		w := where + "." + f.Name
		if !fieldNameRe.MatchString(f.Name) {
			return fmt.Errorf("%s: a field name must start with a lowercase letter and hold only letters, digits and _", w)
		}
		if slices.Contains(seen, f.Name) {
			return fmt.Errorf("%s is declared twice", w)
		}
		seen = append(seen, f.Name)
		if err := checkField(w, f.Field, nested); err != nil {
			return err
		}
	}
	return nil
}

func checkField(where string, f Field, nested bool) error {
	if err := fieldShape(where, f, nested); err != nil {
		return err
	}
	if err := fieldRange(where, f); err != nil {
		return err
	}
	for i, e := range f.Enum {
		if e == "" || slices.Contains(f.Enum[:i], e) {
			return fmt.Errorf("%s: enum values must be non-empty and distinct", where)
		}
	}
	if f.Pattern != "" {
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return fmt.Errorf("%s: pattern: %w", where, err)
		}
	}
	if f.Type == TypeArray {
		return checkField(where+".items", *f.Items, false)
	}
	return checkFields(where, f.Properties, false)
}

// fieldShape checks the field's type takes the keys it sets, in range.
func fieldShape(where string, f Field, nested bool) error {
	scalar := f.Type == TypeString || f.Type == TypeNumber || f.Type == TypeInteger || f.Type == TypeBoolean
	switch {
	case !scalar && f.Type != TypeArray && f.Type != TypeObject:
		return fmt.Errorf("%s: type must be string, number, integer, boolean, array or object, got %q", where, f.Type)
	case !scalar && !nested:
		return fmt.Errorf("%s: an array's items and an object's properties must be string, number, integer or boolean", where)
	case (len(f.Enum) > 0 || f.MaxLength != nil || f.Pattern != "") && f.Type != TypeString:
		return fmt.Errorf("%s: enum, maxLength and pattern apply to a string", where)
	case (f.Minimum != nil || f.Maximum != nil) && f.Type != TypeNumber && f.Type != TypeInteger:
		return fmt.Errorf("%s: minimum and maximum apply to a number or integer", where)
	case f.Type == TypeArray && f.Items == nil:
		return fmt.Errorf("%s: an array needs items", where)
	case f.Type != TypeArray && (f.Items != nil || f.MaxItems != nil):
		return fmt.Errorf("%s: items and maxItems apply to an array", where)
	case (len(f.Properties) > 0) != (f.Type == TypeObject):
		return fmt.Errorf("%s: an object needs properties, and only an object takes them", where)
	}
	return nil
}

// fieldRange checks the field's bounds are in range.
func fieldRange(where string, f Field) error {
	switch {
	case f.MaxLength != nil && (*f.MaxLength < 1 || *f.MaxLength > maxFieldLength):
		return fmt.Errorf("%s: maxLength must be 1 to %d", where, maxFieldLength)
	case f.MaxItems != nil && (*f.MaxItems < 1 || *f.MaxItems > maxFieldItems):
		return fmt.Errorf("%s: maxItems must be 1 to %d", where, maxFieldItems)
	case f.Minimum != nil && f.Maximum != nil && *f.Minimum > *f.Maximum:
		return fmt.Errorf("%s: minimum is above maximum", where)
	}
	return nil
}

func (t *Task) checkActions() error {
	a := t.Actions
	var bound []string
	if c := a.Comment; c != nil {
		switch c.Mode {
		case "", CommentSticky, CommentAppend, CommentNone:
		default:
			return fmt.Errorf("actions.comment.mode must be %s, %s or %s, got %q", CommentSticky, CommentAppend, CommentNone, c.Mode)
		}
		if c.Template != "" {
			if err := checkRefPath(c.Template); err != nil {
				return fmt.Errorf("actions.comment.template: %w", err)
			}
		}
		if c.PostMode() != CommentNone {
			bound = append(bound, ActionComment)
		}
	}
	if l := a.Labels; l != nil {
		bound = append(bound, ActionLabels)
		for i, g := range append(slices.Clone(l.Propose.Add), l.Propose.Remove...) {
			if !validGlob(g) {
				return fmt.Errorf("actions.labels.propose: %q (%d) is not a valid glob", g, i)
			}
		}
		for i, r := range l.Rules {
			if len(r.Add) == 0 && len(r.Remove) == 0 {
				return fmt.Errorf("actions.labels.rules[%d] needs add or remove", i)
			}
		}
	}
	if s := a.State; s != nil {
		bound = append(bound, ActionState)
		for i, r := range s.Rules {
			if r.Close == r.Reopen {
				return fmt.Errorf("actions.state.rules[%d] needs exactly one of close or reopen", i)
			}
		}
	}
	for _, u := range []struct {
		key  string
		spec *UsersSpec
	}{{ActionAssign, a.Assign}, {ActionReviewers, a.Reviewers}} {
		if u.spec == nil {
			continue
		}
		bound = append(bound, u.key)
		for i, login := range u.spec.Propose.Users {
			if !loginRe.MatchString(login) {
				return fmt.Errorf("actions.%s.propose.users[%d] %q is not a login", u.key, i, login)
			}
		}
		for i, r := range u.spec.Rules {
			if len(r.Users) == 0 {
				return fmt.Errorf("actions.%s.rules[%d] needs users", u.key, i)
			}
		}
	}
	if c := a.InlineComments; c != nil {
		bound = append(bound, ActionInlineComments)
		if !slices.ContainsFunc(t.On, Trigger.pullCapable) {
			return errors.New("actions.inlineComments need a trigger that can be about a pull request")
		}
	}
	if i := slices.IndexFunc(t.On, Trigger.subjectless); i >= 0 && len(bound) > 0 {
		return fmt.Errorf("actions.%s writes to an issue or pull request, but on[%d] (raw %q) can fire without one",
			bound[0], i, t.On[i].RawEvent)
	}
	// Parsing the action templates and compiling their guards is what
	// Prepare does with the files in hand; the inline ones need no files.
	if err := (&Prepared{Task: t}).parseInline(); err != nil {
		return fmt.Errorf("actions: %w", err)
	}
	return nil
}

func validGlob(g string) bool {
	return strings.TrimSpace(g) != "" && doublestar.ValidatePattern(g)
}

// checkRefPath rejects a repository path that is empty, absolute or
// escapes the repository root.
func checkRefPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path must not be empty")
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path %q must be relative", p)
	}
	if c := path.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("path %q escapes the repository", p)
	}
	return nil
}

// Files are the repository paths the task reads its templates from: its
// system prompt, prompt and comment template.
func (t *Task) Files() []string {
	var out []string
	for _, p := range []string{t.System, t.Prompt} {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if c := t.Actions.Comment; c != nil && c.Template != "" && !slices.Contains(out, c.Template) {
		out = append(out, c.Template)
	}
	return out
}
