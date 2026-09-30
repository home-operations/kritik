package repoconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/review"
)

func TestParse_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, yaml string }{
		{"unknown key", "foo: bar\n"},
		{"bad ignore glob", "ignore:\n  - \"[\"\n"},
		{"skip is gone", "skip:\n  onlyPaths:\n    - \"**/*.md\"\n"},
		{"bad filter syntax", "filter: \"pr.draft &&\"\n"},
		{"filter not bool", "filter: \"pr.title\"\n"},
		{"absolute rule file", "rules: [{ id: a, file: /etc/passwd }]\n"},
		{"rule file escapes repo", "rules: [{ id: a, file: ../x }]\n"},
		{"rule with both a rule and a file", "rules: [{ id: a, rule: Check., file: x.md }]\n"},
		{"rule with neither", "rules: [{ id: a, paths: [\"**\"] }]\n"},
		{"file rule with a bad glob", "rules: [{ id: a, file: x.md, paths: [\"[\"] }]\n"},
		{"the review block is gone", "review:\n  feedback: minimal\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := Parse([]byte(c.yaml)); err == nil {
				t.Fatalf("Parse(%q) = nil error, want error", c.yaml)
			}
		})
	}
}

func TestParse_Valid(t *testing.T) {
	t.Parallel()

	t.Run("empty doc", func(t *testing.T) {
		t.Parallel()
		f, prg, err := Parse(nil)
		if err != nil {
			t.Fatalf("Parse(nil): %v", err)
		}
		if prg != nil {
			t.Fatalf("Parse(nil) program = %v, want nil", prg)
		}
		if !reflect.DeepEqual(f, File{}) {
			t.Fatalf("Parse(nil) file = %+v, want zero value", f)
		}
	})

	t.Run("all fields", func(t *testing.T) {
		t.Parallel()
		doc := []byte(`enabled: true
filter: '!pr.draft'
ignore:
  - "**/*.md"
rules:
  - { id: house-style, file: docs/instructions.md }
  - { id: sql, file: docs/sql.md, paths: ["**/*.sql"] }
requireSuggestedFix: true
comments:
  summaryTemplate: docs/summary.tmpl
  inlineTemplate: docs/inline.tmpl
`)
		f, prg, err := Parse(doc)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if prg == nil {
			t.Fatal("Parse: program = nil, want a compiled filter")
		}
		if f.Enabled == nil || !*f.Enabled {
			t.Fatalf("Enabled = %v, want true", f.Enabled)
		}
		if f.Filter != "!pr.draft" {
			t.Fatalf("Filter = %q, want %q", f.Filter, "!pr.draft")
		}
		if !slices.Equal(f.Ignore, []string{"**/*.md"}) {
			t.Fatalf("Ignore = %v", f.Ignore)
		}
		if !reflect.DeepEqual(f.Rules, []configfile.Rule{
			{ID: "house-style", File: "docs/instructions.md"}, {ID: "sql", File: "docs/sql.md", Paths: []string{"**/*.sql"}},
		}) {
			t.Fatalf("Rules = %v", f.Rules)
		}
		if f.RequireSuggestedFix == nil || !*f.RequireSuggestedFix {
			t.Fatalf("RequireSuggestedFix = %v, want true", f.RequireSuggestedFix)
		}
		if f.Comments.SummaryTemplate != "docs/summary.tmpl" || f.Comments.InlineTemplate != "docs/inline.tmpl" {
			t.Fatalf("Comments = %+v", f.Comments)
		}
	})
}

func TestActiveContext(t *testing.T) {
	t.Parallel()
	files := []configfile.ContextFile{{Path: "arch.md", Description: "a"}, {Path: "schema.sql", Description: "s", Paths: []string{"**/*.sql"}}}
	if got := ActiveContext(files, []string{"main.go"}); len(got) != 1 || got[0].Path != "arch.md" {
		t.Fatalf("ActiveContext = %v", got)
	}
	if got := ActiveContext(files, []string{"db/0001.sql"}); len(got) != 2 {
		t.Fatalf("ActiveContext = %v", got)
	}
}

// TestActiveRules: a rule applies to a change its paths match, or to any
// when it has none; a file rule carries its file's content, and one whose
// file is missing or blank is left out; the rules past each cap are
// counted, not listed.
func TestActiveRules(t *testing.T) {
	t.Parallel()
	rules := []configfile.Rule{
		{ID: "any", Rule: "Check errors."}, {ID: "sql", Rule: "Use placeholders.", Paths: []string{"**/*.sql"}},
		{ID: "big", Rule: strings.Repeat("x", MaxRulesBytes)}, {ID: "style", File: "docs/style.md"},
		{ID: "gone", File: "docs/gone.md"}, {ID: "blank", File: "docs/blank.md"},
		{ID: "huge", File: "docs/huge.md"}, {ID: "last", Rule: "Name things."},
	}
	files := Files{"docs/style.md": " Short names. \n", "docs/blank.md": "\n", "docs/huge.md": strings.Repeat("y", MaxRuleFileBytes)}
	got, left := ActiveRules(rules, files, []string{"main.go"})
	want := []review.Rule{{ID: "any", Text: "Check errors."}, {ID: "style", Text: "Short names.", File: "docs/style.md"}, {ID: "last", Text: "Name things."}}
	if !reflect.DeepEqual(got, want) || left != 2 {
		t.Fatalf("ActiveRules = %+v, %d left", got, left)
	}
	if got, _ := ActiveRules(rules[:2], files, []string{"db/0001.sql"}); len(got) != 2 {
		t.Fatalf("ActiveRules = %+v", got)
	}
}

func TestFile_Referenced(t *testing.T) {
	t.Parallel()
	f := File{
		Rules:    []configfile.Rule{{ID: "a", File: "docs/a.md"}, {ID: "b", File: "docs/b.md", Paths: []string{"b/**"}}, {ID: "c", Rule: "Check."}},
		Comments: Comments{SummaryTemplate: "docs/a.md", InlineTemplate: "docs/c.md"},
	}
	want := []string{"docs/a.md", "docs/b.md", "docs/c.md"}
	if got := f.Referenced(); !slices.Equal(got, want) {
		t.Fatalf("Referenced() = %v, want %v", got, want)
	}
}

// mapReader builds a Collect read function over an in-memory file set, using
// fs.ErrNotExist for any path not present, the same as a real merge-base tree
// reader would for a path that doesn't exist there.
func mapReader(files map[string][]byte) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		b, ok := files[p]
		if !ok {
			return nil, fmt.Errorf("repoconfig_test: %s: %w", p, fs.ErrNotExist)
		}
		return b, nil
	}
}

func TestCollect(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", MaxFileBytes+1)
	// Four of these fit under MaxTotalBytes; a fifth does not.
	chunk := strings.Repeat("c", 220_000)
	tests := []struct {
		name      string
		src       map[string]string
		paths     []string
		wantFiles []string
		wantNotes []string
	}{
		{name: "nothing to read"},
		{
			name:      "each path read once, in order",
			src:       map[string]string{FileName: "rules: []\n", "docs/a.md": "a", "docs/summary.tmpl": "s"},
			paths:     []string{FileName, "docs/a.md", "docs/summary.tmpl", "docs/a.md", ""},
			wantFiles: []string{FileName, "docs/a.md", "docs/summary.tmpl"},
		},
		{
			name:      "a missing path is noted, the others kept",
			src:       map[string]string{"docs/a.md": "a"},
			paths:     []string{"docs/a.md", "docs/missing.md"},
			wantFiles: []string{"docs/a.md"},
			wantNotes: []string{"docs/missing.md: referenced but not found"},
		},
		{
			name:      "an oversized file is noted",
			src:       map[string]string{"docs/big.md": big, "docs/small.md": "small"},
			paths:     []string{"docs/big.md", "docs/small.md"},
			wantFiles: []string{"docs/small.md"},
			wantNotes: []string{TooLarge("docs/big.md")},
		},
		{
			name:      "files past the total budget are noted",
			src:       map[string]string{"1.md": chunk, "2.md": chunk, "3.md": chunk, "4.md": chunk, "5.md": chunk},
			paths:     []string{"1.md", "2.md", "3.md", "4.md", "5.md"},
			wantFiles: []string{"1.md", "2.md", "3.md", "4.md"},
			wantNotes: []string{fmt.Sprintf("5.md: skipped, would exceed the %d byte total limit", MaxTotalBytes)},
		},
		{
			name:      "an escaping path is noted, not read",
			src:       map[string]string{"../secret": "x"},
			paths:     []string{"../secret"},
			wantNotes: []string{`repoconfig: referenced path "../secret" escapes the repository`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := map[string][]byte{}
			for p, content := range tt.src {
				src[p] = []byte(content)
			}
			files, notes, err := Collect(mapReader(src), tt.paths...)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			got := slices.Sorted(maps.Keys(files))
			want := slices.Sorted(slices.Values(tt.wantFiles))
			if !slices.Equal(got, want) || !slices.Equal(notes, tt.wantNotes) {
				t.Fatalf("files = %v notes = %q, want %v %q", got, notes, want, tt.wantNotes)
			}
			for p, content := range files {
				if content != tt.src[p] {
					t.Errorf("files[%s] is not the file's content", p)
				}
			}
		})
	}

	t.Run("a read error other than not-exist propagates", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("disk on fire")
		read := func(string) ([]byte, error) { return nil, wantErr }
		if _, _, err := Collect(read, "docs/broken.md"); !errors.Is(err, wantErr) {
			t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
		}
	})
}

func TestAllIgnored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ignore  []string
		changed []string
		want    bool
	}{
		{"no globs", nil, []string{"main.go"}, false},
		{"all changed paths ignored", []string{"**/*.md"}, []string{"docs/a.md", "docs/b.md"}, true},
		{"one path not ignored", []string{"**/*.md"}, []string{"docs/a.md", "main.go"}, false},
		{"nothing changed", []string{"**/*.md"}, nil, false},
		{"a lockfile-only change, by the default globs", configfile.DefaultIgnore, []string{"go.sum", "web/package-lock.json"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := AllIgnored(c.ignore, c.changed); got != c.want {
				t.Fatalf("AllIgnored(%v) = %v, want %v", c.changed, got, c.want)
			}
		})
	}
}

func TestInstructions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		files     Files
		paths     []string
		want      []string
		truncated bool
	}{
		{name: "none", files: Files{"a.md": "x"}},
		{
			name: "read in order, trimmed, empty and missing skipped", files: Files{"a.md": " one\n", "b.md": "  ", "c.md": "two"},
			paths: []string{"c.md", "gone.md", "b.md", "a.md"}, want: []string{"two", "one"},
		},
		{
			name:  "capped at a UTF-8 boundary",
			files: Files{"a.md": strings.Repeat("a", MaxInstructionBytes-1) + "é", "b.md": "never seen"},
			paths: []string{"a.md", "b.md"}, want: []string{strings.Repeat("a", MaxInstructionBytes-1)}, truncated: true,
		},
		{
			name:  "the separator counts against the cap",
			files: Files{"a.md": strings.Repeat("a", MaxInstructionBytes-4), "b.md": "bbbb"},
			paths: []string{"a.md", "b.md"}, want: []string{strings.Repeat("a", MaxInstructionBytes-4), "bb"}, truncated: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, truncated := Instructions(tt.files, tt.paths)
			if !slices.Equal(got, tt.want) || truncated != tt.truncated {
				t.Fatalf("Instructions = %d item(s), truncated=%v; want %d, %v", len(got), truncated, len(tt.want), tt.truncated)
			}
		})
	}
}
