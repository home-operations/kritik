// Package repoconfig parses .kritik.yaml, the optional per-repository file
// that lets a repository narrow how kritik reviews it: a filter ANDed with
// the operator's own filter, path globs to ignore, a skip-review rule, and
// review instructions/templates read from the repository itself.
//
// Everything here is read from the merge-base commit (the base branch history
// a PR cannot rewrite), never the PR's own tree, so a PR cannot use its own
// .kritik.yaml to weaken the review applied to it. Collect's read callback
// is how the caller enforces that; this package only decides which paths to
// read and how much of what comes back to keep.
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
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
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

// Review holds the repository's review customizations.
type Review struct {
	Instructions        []string  `yaml:"instructions,omitempty"`
	RequireSuggestedFix *bool     `yaml:"requireSuggestedFix,omitempty"`
	Templates           Templates `yaml:"templates,omitempty"`
}

// Skip decides whether a PR should be skipped outright based on the paths it
// changes.
type Skip struct {
	OnlyPaths []string `yaml:"onlyPaths,omitempty"`
}

// File is the decoded content of .kritik.yaml.
type File struct {
	Enabled *bool    `yaml:"enabled,omitempty"`
	Filter  string   `yaml:"filter,omitempty"`
	Ignore  []string `yaml:"ignore,omitempty"`
	Skip    Skip     `yaml:"skip,omitempty"`
	Review  Review   `yaml:"review,omitempty"`
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
	for _, p := range f.Review.Instructions {
		add(p)
	}
	add(f.Review.Templates.Summary)
	add(f.Review.Templates.Inline)
	return out
}

// Files is the content the runner read from the merge-base tree, keyed by
// repository-relative path. A path Collect could not obtain (missing,
// oversized) is simply absent from the map.
type Files map[string]string

// Collect reads FileName, every path it references, then every path in
// extra (the operator's own review files) through read, which must return
// an error satisfying errors.Is(err, fs.ErrNotExist) for a missing path. A
// file over MaxFileBytes, or one that would push the total over
// MaxTotalBytes, is omitted and reported in the returned notes rather than
// failing the call; so is a missing referenced path and an extra path that
// escapes the repository. A missing FileName is not an error either, since
// the file is optional. Any other read error is returned as-is.
//
// The .kritik.yaml content itself is decoded best-effort to discover
// Referenced() paths: a malformed file is Parse's concern (the caller
// validates separately), not Collect's - Collect still gathers whatever
// context it can.
func Collect(read func(name string) ([]byte, error), extra ...string) (Files, []string, error) {
	files := Files{}
	var notes []string
	var total int

	keep := func(p string, b []byte) {
		if len(b) > MaxFileBytes {
			notes = append(notes, fmt.Sprintf("%s: skipped, it exceeds the %d byte per-file limit", p, MaxFileBytes))
			return
		}
		if total+len(b) > MaxTotalBytes {
			notes = append(notes, fmt.Sprintf("%s: skipped, would exceed the %d byte total limit", p, MaxTotalBytes))
			return
		}
		files[p] = string(b)
		total += len(b)
	}

	var refs []string
	data, err := read(FileName)
	switch {
	case err == nil:
		keep(FileName, data)
		var f File
		_ = yaml.Unmarshal(data, &f)
		refs = f.Referenced()
	case !errors.Is(err, fs.ErrNotExist):
		return nil, nil, fmt.Errorf("repoconfig: read %s: %w", FileName, err)
	}
	for _, p := range extra {
		if p == "" || slices.Contains(refs, p) {
			continue
		}
		if err := validateRefPath(p); err != nil {
			notes = append(notes, err.Error())
			continue
		}
		refs = append(refs, p)
	}

	for _, p := range refs {
		b, err := read(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				notes = append(notes, fmt.Sprintf("%s: referenced but not found", p))
				continue
			}
			return nil, nil, fmt.Errorf("repoconfig: read %s: %w", p, err)
		}
		keep(p, b)
	}

	return files, notes, nil
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
			s = cutUTF8(s, max(room, 0))
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
