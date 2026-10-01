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

// Merged is the admin's settings with the merge-base FileName applied.
type Merged struct {
	configfile.Settings
	// InRepoFilter is the file's own filter, ANDed with the admin's,
	// which ingest has already applied; nil when it sets none.
	InRepoFilter *prfilter.Program
	// Dropped says which of the file's values fell outside the admin's
	// bounds; the admin's value applies for each.
	Dropped []string
}

// Merge applies doc, the merge-base FileName or nil when the repository has
// none, over the admin's settings op. The file narrows what an admin allows
// (enabled, filter, ignore), appends its context files and rules to the
// admin's, may only turn requireSuggestedFix on, and replaces the models,
// the feedback level, how the review comments, whether it approves and
// whether it reads agent files. A model must be one of a provider
// op.Providers names. A value it may not take is dropped, and Dropped says
// so. A file that does not parse is ignored as a whole: op stands, and the
// error says why.
func Merge(doc []byte, op configfile.Settings) (Merged, error) {
	op.Ignore = slices.Clone(op.Ignore)
	op.Review.Context = slices.Clone(op.Review.Context)
	op.Review.Rules = slices.Clone(op.Review.Rules)
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
	if v := f.RequireSuggestedFix; v != nil && *v {
		m.Review.RequireSuggestedFix = true
	} else if v != nil && op.Review.RequireSuggestedFix {
		m.drop("requireSuggestedFix", "false", "true, since an admin requires a suggested fix")
	}
	if f.Comments.SummaryTemplate != nil {
		m.Review.Templates.Summary = *f.Comments.SummaryTemplate
	}
	if f.Comments.InlineTemplate != nil {
		m.Review.Templates.Inline = *f.Comments.InlineTemplate
	}
	if f.Comments.Inline != nil {
		m.Review.InlineComments = *f.Comments.Inline
	}
	if f.Approve != nil {
		m.Review.Approve = *f.Approve
	}
	if f.AgentFiles != nil {
		m.Review.AgentFiles = *f.AgentFiles
	}
	switch {
	case f.Feedback == "":
	case configfile.ValidFeedback(f.Feedback):
		m.Review.Feedback = f.Feedback
	default:
		m.drop("feedback", strconv.Quote(f.Feedback), configfile.FeedbackLevels)
	}
	for _, c := range f.Context {
		if !slices.ContainsFunc(m.Review.Context, func(o configfile.ContextFile) bool { return o.Path == c.Path }) {
			m.Review.Context = append(m.Review.Context, c)
		}
	}
	for _, r := range f.Rules {
		if slices.ContainsFunc(op.Review.Rules, func(o configfile.Rule) bool { return o.ID == r.ID }) {
			m.Dropped = append(m.Dropped, fmt.Sprintf("%s: rules %s was dropped: an admin's rule has that id", FileName, r.ID))
			continue
		}
		m.Review.Rules = append(m.Review.Rules, r)
	}
	m.choose(&f, op.Providers)
	return m, nil
}

// choose applies the models f chooses: a model of one of providers.
func (m *Merged) choose(f *File, providers []string) {
	for _, c := range []struct {
		field string
		want  configfile.ModelRef
		dst   *configfile.ModelRef
	}{
		{"models.review", f.Models.Review, &m.Models.Review},
		{"models.fallback", f.Models.Fallback, &m.Models.Fallback},
	} {
		if c.want == "" {
			continue
		}
		if c.want.Model() != "" && slices.Contains(providers, c.want.Provider()) {
			*c.dst = c.want
		} else {
			m.drop(c.field, strconv.Quote(string(c.want)), "a model of "+list(providers))
		}
	}
}

// drop notes a value the file may not take.
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
		return "only ignored paths changed"
	}
	return string(r)
}

// Check returns why m skips a review of a pull request with the filter
// variables vars that changes changed, or "" when it does not. A filter
// that fails to evaluate skips, since the file may only narrow; the error
// is returned for the log.
func (m *Merged) Check(vars map[string]any, changed []string) (SkipReason, error) {
	if !m.Enabled {
		return SkipDisabled, nil
	}
	if m.InRepoFilter != nil {
		ok, err := m.InRepoFilter.Eval(vars)
		if err != nil || !ok {
			return SkipFiltered, err
		}
	}
	if AllIgnored(m.Ignore, changed) {
		return SkipOnlyPaths, nil
	}
	return "", nil
}

// PullRequest is what a filter sees of a pull request, and what a review
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
