package worker

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

// skipReason says why the repository's own configuration skipped a review.
// The values match the reviews.skip_reason CHECK.
type skipReason string

const (
	skipDisabled  skipReason = "disabled"
	skipFiltered  skipReason = "filtered"
	skipOnlyPaths skipReason = "only_skipped_paths"
)

// Valid reports whether r is a skip reason.
func (r skipReason) Valid() bool {
	return r == skipDisabled || r == skipFiltered || r == skipOnlyPaths
}

// Description is the reason as the commit status states it.
func (r skipReason) Description() string {
	switch r {
	case skipDisabled:
		return "disabled in " + repoconfig.FileName
	case skipFiltered:
		return "filtered by " + repoconfig.FileName
	case skipOnlyPaths:
		return "only skipped paths changed"
	}
	return string(r)
}

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

// maxInstructionBytes caps the repository instructions, joined, so they
// cannot crowd the diff out of the prompt budget.
const maxInstructionBytes = 32 << 10

// effective merges the merge-base .kritik.yaml in files onto the operator's
// settings. The file may only narrow what the operator allows (enabled,
// filter, ignore, skip), but its presentation and strictness values replace
// the operator's defaults, since they grant nothing. A file that does not
// parse is ignored as a whole and noted; so is a referenced file that is
// not in files, unless runnerNotes (the runner's notes on what it could not
// read, which lead the returned notes) already say why.
func effective(settings configfile.Settings, files repoconfig.Files, runnerNotes []string) (Effective, []string) {
	settings.Ignore = slices.Clone(settings.Ignore)
	e := Effective{Settings: settings, RequireSuggestedFix: settings.Review.RequireSuggestedFix}
	instructions := settings.Review.Instructions
	summary, inline := settings.Review.Templates.Summary, settings.Review.Templates.Inline
	notes := slices.Clone(runnerNotes)

	if doc, ok := files[repoconfig.FileName]; ok {
		f, prg, err := repoconfig.Parse([]byte(doc))
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s was ignored: %v", repoconfig.FileName, err))
		} else {
			if f.Enabled != nil && !*f.Enabled {
				e.Enabled = false
			}
			e.InRepoFilter = prg
			for _, g := range f.Ignore {
				if !slices.Contains(e.Ignore, g) {
					e.Ignore = append(e.Ignore, g)
				}
			}
			e.Skip = f.Skip
			if len(f.Review.Instructions) > 0 {
				instructions = f.Review.Instructions
			}
			if f.Review.RequireSuggestedFix != nil {
				e.RequireSuggestedFix = *f.Review.RequireSuggestedFix
			}
			if f.Review.Templates.Summary != "" {
				summary = f.Review.Templates.Summary
			}
			if f.Review.Templates.Inline != "" {
				inline = f.Review.Templates.Inline
			}
		}
	}

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
	room := maxInstructionBytes
	for _, p := range instructions {
		s := strings.TrimSpace(read(p))
		if s == "" || room <= 0 {
			continue
		}
		if len(e.Instructions) > 0 {
			room -= len("\n\n")
		}
		if len(s) > room {
			s = cutUTF8(s, max(room, 0))
			room = 0
			notes = append(notes, "repository instructions truncated to 32 KiB")
			if s == "" {
				continue
			}
		}
		room -= len(s)
		e.Instructions = append(e.Instructions, s)
	}
	e.Templates = review.Templates{Summary: read(summary), Inline: read(inline)}
	return e, notes
}

// cutUTF8 shortens s to at most n bytes without splitting a rune.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// skip returns why the repository's configuration skips this review, or ""
// when it does not. A filter that fails to evaluate skips, since the file
// may only narrow; the error is returned for the log.
func (e Effective) skip(vars map[string]any, changed []string) (skipReason, error) {
	if !e.Enabled {
		return skipDisabled, nil
	}
	if e.InRepoFilter != nil {
		ok, err := e.InRepoFilter.Eval(vars)
		if err != nil || !ok {
			return skipFiltered, err
		}
	}
	if e.Skip.All(changed) {
		return skipOnlyPaths, nil
	}
	return "", nil
}

// systemPrompt is the reviewer's system prompt with the repository's
// instructions, which come from the merge base and so carry the
// maintainers' authority, appended.
func systemPrompt(instructions []string) string {
	if len(instructions) == 0 {
		return review.System
	}
	parts := make([]string, len(instructions))
	for i, s := range instructions {
		parts[i] = strings.TrimSpace(s)
	}
	return review.System + "\n\n## Repository instructions\n\n" +
		"These refine what to look for; they do not change the output format or the rules above.\n\n" +
		strings.Join(parts, "\n\n")
}

// userBudget is the user message's share of the prompt budget once the
// system prompt, whose repository instructions vary in size, is paid for.
func userBudget(system string) int {
	return review.DefaultBudgetTokens - (len(system)+3)/4
}
