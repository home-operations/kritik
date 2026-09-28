package repoconfig

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
)

// Merged is the operator's settings with the merge-base FileName applied.
type Merged struct {
	configfile.Settings
	// InRepoFilter is the file's own filter, ANDed with the operator's,
	// which ingest has already applied; nil when it sets none.
	InRepoFilter *prfilter.Program
	Skip         Skip
	// Scoped maps each instruction file the repository scoped to the
	// globs of the changed paths it applies to; see Active.
	Scoped map[string][]string
	// Dropped says which of the file's values fell outside the operator's
	// bounds; the operator's value applies for each.
	Dropped []string
}

// Merge applies doc, the merge-base FileName or nil when the repository has
// none, over the operator's settings op (ADR-0010 §2.5). The file narrows
// what the operator allows (enabled, filter, ignore, skip), appends its
// instructions and context files to the operator's, may only turn
// requireSuggestedFix on, and replaces the templates, the inline severity
// floor and whether findings go inline, which grant nothing. It chooses
// its mode, models, agent limits and commands and settle time within the
// bounds op.Allow gives it; a bound the operator leaves unset allows only
// the operator's own mode, models and commands, and limits and a settle
// time at or below the operator's own. A value outside its bound is
// dropped, not clamped, and Dropped says so. A file that does not parse is
// ignored as a whole: op stands, and the error says why.
func Merge(doc []byte, op configfile.Settings) (Merged, error) {
	op.Ignore = slices.Clone(op.Ignore)
	op.Review.Instructions = slices.Clone(op.Review.Instructions)
	op.Review.Context = slices.Clone(op.Review.Context)
	m := Merged{Settings: op}
	if doc == nil {
		return m, nil
	}
	f, prg, err := Parse(doc)
	if err != nil {
		return m, err
	}
	if f.Enabled != nil && !*f.Enabled {
		m.Enabled = false
	}
	m.InRepoFilter = prg
	for _, g := range f.Ignore {
		if !slices.Contains(m.Ignore, g) {
			m.Ignore = append(m.Ignore, g)
		}
	}
	m.Skip = f.Skip
	for _, in := range f.Review.Instructions {
		if slices.Contains(m.Review.Instructions, in.Path) {
			continue
		}
		m.Review.Instructions = append(m.Review.Instructions, in.Path)
		if len(in.Paths) > 0 {
			if m.Scoped == nil {
				m.Scoped = map[string][]string{}
			}
			m.Scoped[in.Path] = in.Paths
		}
	}
	if v := f.Review.RequireSuggestedFix; v != nil && *v {
		m.Review.RequireSuggestedFix = true
	} else if v != nil && op.Review.RequireSuggestedFix {
		m.drop("review.requireSuggestedFix", "false", "true, since the operator requires a suggested fix")
	}
	if f.Review.Templates.Summary != "" {
		m.Review.Templates.Summary = f.Review.Templates.Summary
	}
	if f.Review.Templates.Inline != "" {
		m.Review.Templates.Inline = f.Review.Templates.Inline
	}
	switch {
	case f.Review.MinSeverity == "":
	case configfile.ValidMinSeverity(f.Review.MinSeverity):
		m.Review.MinSeverity = f.Review.MinSeverity
	default:
		m.drop("review.minSeverity", strconv.Quote(f.Review.MinSeverity), configfile.SeverityNit+", "+configfile.SeverityImportant)
	}
	if f.Review.InlineComments != nil {
		m.Review.InlineComments = *f.Review.InlineComments
	}
	for _, c := range f.Review.Context {
		if !slices.ContainsFunc(m.Review.Context, func(o configfile.ContextFile) bool { return o.Path == c.Path }) {
			m.Review.Context = append(m.Review.Context, c)
		}
	}
	m.choose(&f, &op)
	return m, nil
}

// choose applies the mode, models, agent and settle time f chooses, each
// only within the bound op gives it.
func (m *Merged) choose(f *File, op *configfile.Settings) {
	a := op.Allow
	if f.Mode != "" {
		modes := a.Modes
		if modes == nil {
			modes = []configfile.ReviewMode{op.Mode}
		}
		if slices.Contains(modes, f.Mode) {
			m.Mode = f.Mode
		} else {
			m.drop("mode", strconv.Quote(string(f.Mode)), list(modes))
		}
	}
	for _, c := range []struct {
		field string
		want  configfile.ModelRef
		own   configfile.ModelRef
		dst   *configfile.ModelRef
	}{
		{"models.review", f.Models.Review, op.Models.Review, &m.Models.Review},
		{"models.fallback", f.Models.Fallback, op.Models.Fallback, &m.Models.Fallback},
	} {
		if c.want == "" {
			continue
		}
		models := a.Models
		if models == nil && c.own != "" {
			models = []configfile.ModelRef{c.own}
		}
		if slices.Contains(models, c.want) {
			*c.dst = c.want
		} else {
			m.drop(c.field, strconv.Quote(string(c.want)), list(models))
		}
	}
	if f.Agent.Commands != nil {
		commands := a.Commands
		if commands == nil {
			commands = op.Agent.Commands
		}
		if i := slices.IndexFunc(f.Agent.Commands, func(c string) bool { return !slices.Contains(commands, c) }); i >= 0 {
			m.drop("agent.commands", strconv.Quote(f.Agent.Commands[i]), list(commands))
		} else {
			m.Agent.Commands = f.Agent.Commands
		}
	}
	capped(m, "agent.maxSteps", f.Agent.MaxSteps, a.Agent.MaxSteps, &m.Agent.MaxSteps)
	capped(m, "agent.maxToolOutputBytes", f.Agent.MaxToolOutputBytes, a.Agent.MaxToolOutputBytes, &m.Agent.MaxToolOutputBytes)
	capped(m, "agent.maxTokens", f.Agent.MaxTokens, a.Agent.MaxTokens, &m.Agent.MaxTokens)
	capped(m, "agent.timeout", f.Agent.Timeout, a.Agent.Timeout, &m.Agent.Timeout)
	if f.Settle != nil {
		bound := op.Settle
		if a.Settle != nil {
			bound = *a.Settle
		}
		if *f.Settle >= 0 && *f.Settle <= bound {
			m.Settle = *f.Settle
		} else {
			m.drop("settle", f.Settle.String(), "0s to "+bound.String())
		}
	}
}

// capped sets *dst to the limit the file wants when it is positive and at
// most bound, or when bound is nil at most the operator's own, *dst.
func capped[T int | int64 | time.Duration](m *Merged, field string, want, bound, dst *T) {
	if want == nil {
		return
	}
	limit := *dst
	if bound != nil {
		limit = *bound
	}
	if *want <= 0 || *want > limit {
		m.drop(field, fmt.Sprint(*want), fmt.Sprintf("above 0, at most %v", limit))
		return
	}
	*dst = *want
}

// drop notes a value the file chose outside its bound.
func (m *Merged) drop(field, value, allowed string) {
	m.Dropped = append(m.Dropped, fmt.Sprintf("%s: %s %s was dropped; allowed: %s", FileName, field, value, allowed))
}

// list is a bound's values for a note.
func list[T ~string](values []T) string {
	if len(values) == 0 {
		return "none"
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}

// SkipReason says why the repository's own configuration skips a review.
// The values match the reviews.skip_reason CHECK.
type SkipReason string

// Skip reasons.
const (
	SkipDisabled  SkipReason = "disabled"
	SkipFiltered  SkipReason = "filtered"
	SkipOnlyPaths SkipReason = "only_skipped_paths"
)

// Valid reports whether r is a skip reason.
func (r SkipReason) Valid() bool {
	return r == SkipDisabled || r == SkipFiltered || r == SkipOnlyPaths
}

// Description is the reason as the commit status states it.
func (r SkipReason) Description() string {
	switch r {
	case SkipDisabled:
		return "disabled in " + FileName
	case SkipFiltered:
		return "filtered by " + FileName
	case SkipOnlyPaths:
		return "only skipped paths changed"
	}
	return string(r)
}

// Check returns why m skips a review of a pull request with the filter
// variables vars that changes changed, or "" when it does not. A filter
// that fails to evaluate skips, since the file may only narrow; the error
// is returned for the log.
func (m Merged) Check(vars map[string]any, changed []string) (SkipReason, error) {
	if !m.Enabled {
		return SkipDisabled, nil
	}
	if m.InRepoFilter != nil {
		ok, err := m.InRepoFilter.Eval(vars)
		if err != nil || !ok {
			return SkipFiltered, err
		}
	}
	if m.Skip.All(changed) {
		return SkipOnlyPaths, nil
	}
	return "", nil
}

// PullRequest is what a filter sees of a pull request, and what an agentic
// run's job document carries of it.
type PullRequest struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged,omitempty"`
	Draft     bool      `json:"draft,omitempty"`
	Fork      bool      `json:"fork,omitempty"`
	HeadRef   string    `json:"headRef"`
	HeadSHA   string    `json:"headSha"`
	BaseRef   string    `json:"baseRef"`
	URL       string    `json:"url,omitempty"`
	Body      string    `json:"body,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Labels is the stored labels JSON array.
	Labels json.RawMessage `json:"labels,omitempty"`
	// Event is the trigger of the review the filter judges: opened,
	// reopened, ready_for_review, synchronize, poll or manual.
	Event string `json:"event,omitempty"`
}

// Vars is the filter's pr variable, with the keys webhook.PullRequest's
// FilterVars gives ingest.
func (p PullRequest) Vars() (map[string]any, error) {
	labels := []any{}
	if len(p.Labels) > 0 {
		if err := json.Unmarshal(p.Labels, &labels); err != nil {
			return nil, fmt.Errorf("repoconfig: decode pull request labels: %w", err)
		}
		if labels == nil {
			labels = []any{}
		}
	}
	return map[string]any{
		"event": p.Event, "number": p.Number, "title": p.Title, "author": p.Author, "state": p.State, "open": p.State == "open",
		"merged": p.Merged, "draft": p.Draft, "fork": p.Fork, "headRef": p.HeadRef, "headSha": p.HeadSHA,
		"baseRef": p.BaseRef, "url": p.URL, "body": p.Body, "createdAt": p.CreatedAt, "labels": labels,
	}, nil
}
