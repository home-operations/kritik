// Package repoconfig parses .kritik.yaml, the optional per-repository file
// that lets a repository narrow how kritik reviews it (a filter ANDed with
// the operator's own filter, path globs to ignore, a skip-review rule),
// add review instructions and templates read from the repository itself,
// and choose its mode, models, agent limits and settle time within the
// bounds the operator allows.
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
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
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

// MaxInstructionBytes caps the repository instructions, joined, so they
// cannot crowd the diff out of the prompt budget.
const MaxInstructionBytes = 32 << 10

// Templates names in-repo files whose contents replace kritik's built-in
// summary/inline comment templates.
type Templates struct {
	Summary string `yaml:"summary,omitempty"`
	Inline  string `yaml:"inline,omitempty"`
}

// Instruction is a file of review instructions in the repository: always
// included, or with Paths only when a changed path matches one of them.
type Instruction struct {
	Path  string
	Paths []string
}

// UnmarshalYAML takes a bare path, or a mapping of path and paths.
func (in *Instruction) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&in.Path)
	}
	// Node.Decode drops the strictness Parse asked for, so unknown keys are
	// refused here.
	for i := 0; n.Kind == yaml.MappingNode && i < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Value != "path" && k.Value != "paths" {
			return fmt.Errorf("line %d: field %s not found in type repoconfig.Instruction", k.Line, k.Value)
		}
	}
	var v struct {
		Path  string   `yaml:"path"`
		Paths []string `yaml:"paths"`
	}
	if err := n.Decode(&v); err != nil {
		return err
	}
	in.Path, in.Paths = v.Path, v.Paths
	return nil
}

// Review holds the repository's review customizations.
type Review struct {
	Instructions        []Instruction `yaml:"instructions,omitempty"`
	RequireSuggestedFix *bool         `yaml:"requireSuggestedFix,omitempty"`
	Templates           Templates     `yaml:"templates,omitempty"`
	MinSeverity         string        `yaml:"minSeverity,omitempty"`
	InlineComments      *bool         `yaml:"inlineComments,omitempty"`
	// Context names files that explain the code, added after the
	// operator's.
	Context []configfile.ContextFile `yaml:"context,omitempty"`
}

// Skip decides whether a PR should be skipped outright based on the paths it
// changes.
type Skip struct {
	OnlyPaths []string `yaml:"onlyPaths,omitempty"`
}

// Models are the review and fallback models a repository chooses, each a
// "<provider>/<model>" the operator's bounds list.
type Models struct {
	Review   configfile.ModelRef `yaml:"review,omitempty"`
	Fallback configfile.ModelRef `yaml:"fallback,omitempty"`
}

// Agent is the agent limits and commands a repository chooses.
type Agent struct {
	MaxSteps           *int           `yaml:"maxSteps,omitempty"`
	MaxToolOutputBytes *int           `yaml:"maxToolOutputBytes,omitempty"`
	MaxTokens          *int64         `yaml:"maxTokens,omitempty"`
	Timeout            *time.Duration `yaml:"timeout,omitempty"`
	Commands           []string       `yaml:"commands,omitempty"`
}

// File is the decoded content of .kritik.yaml. Nothing in it is a secret or
// a reference to one: it can only name what the operator configured.
type File struct {
	Enabled *bool                 `yaml:"enabled,omitempty"`
	Mode    configfile.ReviewMode `yaml:"mode,omitempty"`
	Models  Models                `yaml:"models,omitempty"`
	Agent   Agent                 `yaml:"agent,omitempty"`
	Settle  *time.Duration        `yaml:"settle,omitempty"`
	Filter  string                `yaml:"filter,omitempty"`
	Ignore  []string              `yaml:"ignore,omitempty"`
	Skip    Skip                  `yaml:"skip,omitempty"`
	Review  Review                `yaml:"review,omitempty"`
}

// Parse decodes data as .kritik.yaml. Unknown fields, invalid glob patterns
// and a filter that fails to compile or that fails a smoke test against
// configfile.SamplePR are rejected, as is any referenced path (an
// instruction or template) that is absolute or escapes the repository via
// "..". An empty document is valid (the file is optional) and yields a zero
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
		if !validGlob(g) {
			return File{}, nil, fmt.Errorf("repoconfig: ignore[%d] %q is not a valid glob", i, g)
		}
	}
	for i, g := range f.Skip.OnlyPaths {
		if !validGlob(g) {
			return File{}, nil, fmt.Errorf("repoconfig: skip.onlyPaths[%d] %q is not a valid glob", i, g)
		}
	}
	for i, c := range f.Review.Context {
		if err := c.Check(); err != nil {
			return File{}, nil, fmt.Errorf("repoconfig: review.context[%d]: %w", i, err)
		}
	}
	for i, in := range f.Review.Instructions {
		if in.Path == "" {
			return File{}, nil, fmt.Errorf("repoconfig: review.instructions[%d] needs a path", i)
		}
		for j, g := range in.Paths {
			if !validGlob(g) {
				return File{}, nil, fmt.Errorf("repoconfig: review.instructions[%d].paths[%d] %q is not a valid glob", i, j, g)
			}
		}
	}
	for _, p := range f.Referenced() {
		if err := validateRefPath(p); err != nil {
			return File{}, nil, err
		}
	}

	var prg *prfilter.Program
	if strings.TrimSpace(f.Filter) != "" {
		var err error
		prg, err = prfilter.Compile(f.Filter)
		if err != nil {
			return File{}, nil, fmt.Errorf("repoconfig: filter: %w", err)
		}
		if _, err := prg.Eval(configfile.SamplePR()); err != nil {
			return File{}, nil, fmt.Errorf("repoconfig: filter: smoke test against a sample pull request: %w", err)
		}
	}

	return f, prg, nil
}

func validGlob(g string) bool {
	return strings.TrimSpace(g) != "" && doublestar.ValidatePattern(g)
}

// validateRefPath rejects a referenced path that is absolute or that, once
// cleaned, escapes the repository root - both are read through Collect's
// caller-supplied read function, so an unbounded path would let a
// repository's own config read arbitrary files on the runner's checkout.
func validateRefPath(p string) error {
	if path.IsAbs(p) {
		return fmt.Errorf("repoconfig: referenced path %q must be relative", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("repoconfig: referenced path %q escapes the repository", p)
	}
	return nil
}

// Referenced lists the in-repo paths the file names: review instructions
// first, then the summary and inline templates, deduplicated in the order
// first seen.
func (f File) Referenced() []string {
	seen := make(map[string]bool, len(f.Review.Instructions)+2)
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, in := range f.Review.Instructions {
		add(in.Path)
	}
	add(f.Review.Templates.Summary)
	add(f.Review.Templates.Inline)
	for _, c := range f.Review.Context {
		add(c.Path)
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
			notes = append(notes, fmt.Sprintf("%s: skipped, would exceed the %d byte total limit", p, MaxTotalBytes))
		default:
			files[p] = string(b)
			total += len(b)
		}
	}
	return files, notes, nil
}

// TooLarge is the note for a file over MaxFileBytes.
func TooLarge(name string) string {
	return fmt.Sprintf("%s: skipped, it exceeds the %d byte per-file limit", name, MaxFileBytes)
}

// All reports whether every path in changed matches at least one of s's
// OnlyPaths glob patterns. It is false when there are no patterns or no
// changed paths - an empty rule skips nothing, and there is nothing to
// judge a skip against.
func (s Skip) All(changed []string) bool {
	if len(s.OnlyPaths) == 0 || len(changed) == 0 {
		return false
	}
	for _, c := range changed {
		if !matchesAny(s.OnlyPaths, c) {
			return false
		}
	}
	return true
}

func matchesAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if ok, _ := doublestar.Match(pat, p); ok {
			return true
		}
	}
	return false
}

// Active is the instruction paths that apply to a change of the changed
// paths, in order: every path scoped does not name, and each one it does
// when a changed path matches one of its globs.
func Active(paths []string, scoped map[string][]string, changed []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if globs, ok := scoped[p]; ok && !slices.ContainsFunc(changed, func(c string) bool { return matchesAny(globs, c) }) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ActiveContext is the context files that apply to a change of the changed
// paths, in order: each without paths, and each with them when a changed
// path matches one.
func ActiveContext(files []configfile.ContextFile, changed []string) []configfile.ContextFile {
	var out []configfile.ContextFile
	for _, f := range files {
		if len(f.Paths) == 0 || slices.ContainsFunc(changed, func(c string) bool { return matchesAny(f.Paths, c) }) {
			out = append(out, f)
		}
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
