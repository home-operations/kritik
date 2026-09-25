package repoconfig

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/home-operations/kritik/internal/prfilter"
)

// Operator is the operator's side of the settings the merge-base FileName
// may change for one repository. Instructions and Templates name files.
type Operator struct {
	Enabled             bool
	Ignore              []string
	Instructions        []string
	RequireSuggestedFix bool
	Templates           Templates
}

// Merged is the operator's settings with the merge-base FileName applied.
type Merged struct {
	Operator
	// Filter is the file's own filter, ANDed with the operator's, which
	// ingest has already applied; nil when it sets none.
	Filter *prfilter.Program
	Skip   Skip
}

// Merge applies the merge-base FileName in files over op. The file may only
// narrow what the operator allows (enabled, filter, ignore, skip), but its
// presentation and strictness values replace the operator's defaults,
// since they grant nothing. A file that does not parse is ignored as a
// whole: op stands, and the error says why.
func Merge(files Files, op Operator) (Merged, error) {
	m := Merged{Operator: op}
	m.Ignore = slices.Clone(op.Ignore)
	doc, ok := files[FileName]
	if !ok {
		return m, nil
	}
	f, prg, err := Parse([]byte(doc))
	if err != nil {
		return m, err
	}
	if f.Enabled != nil && !*f.Enabled {
		m.Enabled = false
	}
	m.Filter = prg
	for _, g := range f.Ignore {
		if !slices.Contains(m.Ignore, g) {
			m.Ignore = append(m.Ignore, g)
		}
	}
	m.Skip = f.Skip
	if len(f.Review.Instructions) > 0 {
		m.Instructions = f.Review.Instructions
	}
	if f.Review.RequireSuggestedFix != nil {
		m.RequireSuggestedFix = *f.Review.RequireSuggestedFix
	}
	if f.Review.Templates.Summary != "" {
		m.Templates.Summary = f.Review.Templates.Summary
	}
	if f.Review.Templates.Inline != "" {
		m.Templates.Inline = f.Review.Templates.Inline
	}
	return m, nil
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

func (r SkipReason) String() string { return string(r) }

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
	if m.Filter != nil {
		ok, err := m.Filter.Eval(vars)
		if err != nil || !ok {
			return SkipFiltered, err
		}
	}
	if m.Skip.All(changed) {
		return SkipOnlyPaths, nil
	}
	return "", nil
}

// PullRequest is what a filter sees of a pull request. It crosses the
// runner's job document as JSON, and Vars rebuilds the same pr variable on
// either side.
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
		"number": p.Number, "title": p.Title, "author": p.Author, "state": p.State, "open": p.State == "open",
		"merged": p.Merged, "draft": p.Draft, "fork": p.Fork, "headRef": p.HeadRef, "headSha": p.HeadSHA,
		"baseRef": p.BaseRef, "url": p.URL, "body": p.Body, "createdAt": p.CreatedAt, "labels": labels,
	}, nil
}
