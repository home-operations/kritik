// Package repoconfig parses .kritik.yaml, the optional per-repository file
// that lets a repository narrow how kritik reviews it (a filter ANDed with
// the admin's own filter, path globs to ignore, which also skip a pull
// request that changes nothing else),
// add rules, context files and templates read from the repository itself,
// and choose its models among what its account may use.
//
// Everything here is read from the merge-base commit (the base branch history
// a PR cannot rewrite), never the PR's own tree, so a PR cannot use its own
// .kritik.yaml to weaken the review applied to it. The worker reads the
// file itself and hands it to Merge; Collect's read callback is how the
// runner reads the files it names from the same commit. This package only
// decides which paths to read and how much of what comes back to keep.
package repoconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/chunk"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/textcut"
)

// FileName is the repository-relative path of the per-repository config file.
const FileName = ".kritik.yaml"

// Byte budgets for Collect. A repository config is meant to point at a
// handful of small instruction/template files, not embed arbitrary content;
// these caps bound how much of the merge-base tree ends up in a review
// prompt.
const (
	MaxFileBytes  = 256 << 10
	MaxTotalBytes = 1 << 20
)

// MaxInstructionBytes caps the repository instructions, its agent files
// joined, so they cannot crowd the diff out of the prompt budget.
const MaxInstructionBytes = 32 << 10

// MaxRulesBytes caps the rules a prompt lists by their ids and text, and
// MaxRuleFileBytes the content of its file rules, for the same reason.
const (
	MaxRulesBytes    = 16 << 10
	MaxRuleFileBytes = 32 << 10
)

// Comments is how the repository's reviews comment: whether findings go
// inline, and the in-repo files whose contents replace kritik's built-in
// summary and inline comment templates.
type Comments struct {
	Inline *bool `yaml:"inline,omitempty"`
	// SummaryTemplate and InlineTemplate replace the admin's; an empty
	// path restores the built-in template.
	SummaryTemplate *string `yaml:"summaryTemplate,omitempty"`
	InlineTemplate  *string `yaml:"inlineTemplate,omitempty"`
}

// Models are the review and fallback models a repository chooses, each a
// "<provider>/<model>" of a provider its account may use.
type Models struct {
	Review   configfile.ModelRef `yaml:"review,omitempty"`
	Fallback configfile.ModelRef `yaml:"fallback,omitempty"`
}

// File is the decoded content of .kritik.yaml: the review keys the
// configuration's defaults and repository entries take, without the admin's
// own. Nothing in it is a secret or a reference to one.
type File struct {
	Enabled             *bool    `yaml:"enabled,omitempty"`
	Models              Models   `yaml:"models,omitempty"`
	Feedback            string   `yaml:"feedback,omitempty"`
	Comments            Comments `yaml:"comments,omitempty"`
	RequireSuggestedFix *bool    `yaml:"requireSuggestedFix,omitempty"`
	// Approve replaces the admin's: whether a review that finds nothing
	// blocking or important approves the pull request.
	Approve    *bool    `yaml:"approve,omitempty"`
	FilterExpr string   `yaml:"filterExpr,omitempty"`
	Ignore     []string `yaml:"ignore,omitempty"`
	// Rules are checks added after the admin's; one may not replace an
	// admin's rule.
	Rules []configfile.Rule `yaml:"rules,omitempty"`
	// Context names files that explain the code, added after the admin's.
	Context []configfile.ContextFile `yaml:"context,omitempty"`
	// AgentFiles replaces the admin's: whether AGENTS.md and CLAUDE.md
	// files join the instructions.
	AgentFiles *bool `yaml:"agentFiles,omitempty"`
}

// Parse decodes data as .kritik.yaml. Unknown fields, invalid glob patterns
// and a filter that fails to compile or that fails a smoke test against
// configfile.SamplePR are rejected, as is any referenced path (a rule's
// file, a template or a context file) that is absolute or escapes the
// repository via "..". An empty document is valid (the file is optional) and yields a zero
// File with no filter.
func Parse(data []byte) (File, *prfilter.Program, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return File{}, nil, nil
		}
		return File{}, nil, fmt.Errorf("repoconfig: parse: %w", err)
	}

	for i, g := range f.Ignore {
		if !configfile.ValidGlob(g) {
			return File{}, nil, fmt.Errorf("repoconfig: ignore[%d] %q is not a valid glob", i, g)
		}
	}
	for i, c := range f.Context {
		if err := c.Check(); err != nil {
			return File{}, nil, fmt.Errorf("repoconfig: context[%d]: %w", i, err)
		}
	}
	if err := configfile.CheckRules(f.Rules); err != nil {
		return File{}, nil, fmt.Errorf("repoconfig: %w", err)
	}
	for _, p := range f.Referenced() {
		if err := validateRefPath(p); err != nil {
			return File{}, nil, err
		}
	}

	var prg *prfilter.Program
	if strings.TrimSpace(f.FilterExpr) != "" {
		var err error
		prg, err = prfilter.Compile(f.FilterExpr)
		if err != nil {
			return File{}, nil, fmt.Errorf("repoconfig: filterExpr: %w", err)
		}
		if _, err := prg.Eval(configfile.SamplePR()); err != nil {
			return File{}, nil, fmt.Errorf("repoconfig: filterExpr: smoke test against a sample pull request: %w", err)
		}
	}

	return f, prg, nil
}

// validateRefPath rejects a referenced path that is absolute or escapes
// the repository root: every path is read through Collect's caller-supplied
// read function, so an unbounded one would let a repository's own config
// read arbitrary files on the runner's checkout.
func validateRefPath(p string) error {
	if err := configfile.CheckRepoPath(p); err != nil {
		return fmt.Errorf("repoconfig: referenced %w", err)
	}
	return nil
}

// Referenced lists the in-repo paths the file names whose contents a
// review reads: its rules' files first, then the summary and inline
// templates, deduplicated in the order first seen. Context files are
// pointers the agent follows with its tools.
func (f File) Referenced() []string {
	seen := make(map[string]bool, len(f.Rules)+2)
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, r := range f.Rules {
		add(r.File)
	}
	for _, t := range []*string{f.Comments.SummaryTemplate, f.Comments.InlineTemplate} {
		if t != nil {
			add(*t)
		}
	}
	return out
}

// Files is the content the runner read from the merge-base tree, keyed by
// repository-relative path. A path Collect could not obtain (missing,
// oversized) is simply absent from the map.
type Files map[string]string

// Collect reads each of paths through read, which must return an error
// satisfying errors.Is(err, fs.ErrNotExist) for a missing path. A path
// that escapes the repository or is missing, and a file over MaxFileBytes
// or one that would push the total over MaxTotalBytes, is left out and
// reported in the returned notes rather than failing the call. Any other
// read error is returned as-is.
func Collect(read func(name string) ([]byte, error), paths ...string) (Files, []string, error) {
	files := Files{}
	var notes []string
	var total int
	for _, p := range paths {
		if _, seen := files[p]; seen || p == "" {
			continue
		}
		if err := validateRefPath(p); err != nil {
			notes = append(notes, err.Error())
			continue
		}
		b, err := read(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			notes = append(notes, fmt.Sprintf("%s: referenced but not found", p))
		case err != nil:
			return nil, nil, fmt.Errorf("repoconfig: read %s: %w", p, err)
		case len(b) > MaxFileBytes:
			notes = append(notes, TooLarge(p))
		case total+len(b) > MaxTotalBytes:
			notes = append(notes, overTotal(p))
		default:
			files[p] = string(b)
			total += len(b)
		}
	}
	return files, notes, nil
}

// overTotal is the note for a file that would push the total over
// MaxTotalBytes.
func overTotal(name string) string {
	return fmt.Sprintf("%s: skipped, would exceed the %d byte total limit", name, MaxTotalBytes)
}

// TooLarge is the note for a file over MaxFileBytes.
func TooLarge(name string) string {
	return fmt.Sprintf("%s: skipped, it exceeds the %d byte per-file limit", name, MaxFileBytes)
}

// AllIgnored reports whether every path in changed matches one of the
// ignore globs, so the pull request is skipped. It is false when nothing
// changed: there is nothing to judge a skip against.
func AllIgnored(ignore, changed []string) bool {
	if len(ignore) == 0 || len(changed) == 0 {
		return false
	}
	for _, c := range changed {
		if !chunk.Ignored(ignore, c) {
			return false
		}
	}
	return true
}

// ActiveContext is the context files that apply to a change of the changed
// paths, in order: each without paths, and each with them when a changed
// path matches one.
func ActiveContext(files []configfile.ContextFile, changed []string) []configfile.ContextFile {
	var out []configfile.ContextFile
	for _, f := range files {
		if len(f.Paths) == 0 || slices.ContainsFunc(changed, func(c string) bool { return chunk.Ignored(f.Paths, c) }) {
			out = append(out, f)
		}
	}
	return out
}

// ActiveRules is the rules that apply to a change of the changed paths, in
// order: each without paths, and each with them when a changed path
// matches one. A rule's text counts against MaxRulesBytes and a file
// rule's content, read from files, against MaxRuleFileBytes; left is how
// many applied but did not fit. A file rule whose file files lacks or
// holds nothing is left out, since reading it was already noted.
func ActiveRules(rules []configfile.Rule, files Files, changed []string) (out []review.Rule, left int) {
	room, fileRoom := MaxRulesBytes, MaxRuleFileBytes
	for _, r := range rules {
		if len(r.Paths) > 0 && !slices.ContainsFunc(changed, func(c string) bool { return chunk.Ignored(r.Paths, c) }) {
			continue
		}
		text, budget := r.Rule, &room
		if r.File != "" {
			if text, budget = strings.TrimSpace(files[r.File]), &fileRoom; text == "" {
				continue
			}
		}
		if size := len(r.ID) + len(text); size <= *budget {
			*budget -= size
			out = append(out, review.Rule{ID: r.ID, Text: text, File: r.File})
		} else {
			left++
		}
	}
	return out, left
}

// RulesFor is the rules whose whenExpr, if any, is true of vars, the
// filter's pr variable, in order. One that does not compile or evaluate is
// left out, as it could not say the rule applies.
func RulesFor(rules []configfile.Rule, vars map[string]any) []configfile.Rule {
	var out []configfile.Rule
	for _, r := range rules {
		if r.WhenExpr != "" {
			prg, err := prfilter.Compile(r.WhenExpr)
			if err != nil {
				continue
			}
			if ok, err := prg.Eval(vars); err != nil || !ok {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// Instructions returns the contents of the named files, trimmed and in
// order, skipping any that are absent or blank, so that joined by blank
// lines they fit MaxInstructionBytes. truncated reports that the cap cut
// them short.
func Instructions(files Files, paths []string) (out []string, truncated bool) {
	room := MaxInstructionBytes
	for _, p := range paths {
		s := strings.TrimSpace(files[p])
		if s == "" || room <= 0 {
			continue
		}
		if len(out) > 0 {
			room -= len("\n\n")
		}
		if len(s) > room {
			s = textcut.Prefix(s, max(room, 0))
			room, truncated = 0, true
			if s == "" {
				continue
			}
		}
		room -= len(s)
		out = append(out, s)
	}
	return out, truncated
}
