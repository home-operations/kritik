package worker

import (
	"fmt"
	"slices"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

// Effective is a repository's settings once its .kritik.yaml is applied.
// Instructions and Templates hold file contents, not paths.
type Effective struct {
	configfile.Settings
	// InRepoFilter is ANDed with the operator's filter, which ingest has
	// already applied.
	InRepoFilter        *prfilter.Program
	Skip                repoconfig.Skip
	Instructions        []string
	Templates           review.Templates
	RequireSuggestedFix bool
}

// effective merges the merge-base .kritik.yaml in files onto the operator's
// settings. The file may only narrow what the operator allows (enabled,
// filter, ignore, skip), but its presentation and strictness values replace
// the operator's defaults, since they grant nothing. A file that does not
// parse is ignored as a whole and noted; so is a referenced file that is
// not in files, unless runnerNotes (the runner's notes on what it could not
// read, which lead the returned notes) already say why.
func effective(settings configfile.Settings, files repoconfig.Files, runnerNotes []string) (Effective, []string) {
	notes := slices.Clone(runnerNotes)
	m, err := repoconfig.Merge(files, repoconfig.Operator{
		Enabled: settings.Enabled, Ignore: settings.Ignore, Instructions: settings.Review.Instructions,
		RequireSuggestedFix: settings.Review.RequireSuggestedFix,
		Templates:           repoconfig.Templates{Summary: settings.Review.Templates.Summary, Inline: settings.Review.Templates.Inline},
	})
	if err != nil {
		notes = append(notes, fmt.Sprintf("%s was ignored: %v", repoconfig.FileName, err))
	}
	settings.Enabled, settings.Ignore = m.Enabled, m.Ignore
	e := Effective{Settings: settings, InRepoFilter: m.Filter, Skip: m.Skip, RequireSuggestedFix: m.RequireSuggestedFix}
	instructions, summary, inline := m.Instructions, m.Templates.Summary, m.Templates.Inline

	read := func(p string) string {
		if p == "" {
			return ""
		}
		content, ok := files[p]
		if !ok && !slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, p+": ") }) {
			notes = append(notes, fmt.Sprintf("%s: referenced but not found", p))
		}
		return content
	}
	for _, p := range instructions {
		read(p)
	}
	var truncated bool
	if e.Instructions, truncated = repoconfig.Instructions(files, instructions); truncated {
		notes = append(notes, "repository instructions truncated to 32 KiB")
	}
	e.Templates = review.Templates{Summary: read(summary), Inline: read(inline)}
	return e, notes
}

// skip returns why the repository's configuration skips this review, or ""
// when it does not; see repoconfig.Merged.Check.
func (e Effective) skip(vars map[string]any, changed []string) (repoconfig.SkipReason, error) {
	return repoconfig.Merged{Operator: repoconfig.Operator{Enabled: e.Enabled}, Filter: e.InRepoFilter, Skip: e.Skip}.Check(vars, changed)
}
