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
)

func TestParse_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, yaml string }{
		{"unknown key", "foo: bar\n"},
		{"bad ignore glob", "ignore:\n  - \"[\"\n"},
		{"bad skip glob", "skip:\n  onlyPaths:\n    - \"[\"\n"},
		{"bad filter syntax", "filter: \"pr.draft &&\"\n"},
		{"filter not bool", "filter: \"pr.title\"\n"},
		{"absolute instruction path", "review:\n  instructions:\n    - /etc/passwd\n"},
		{"instruction escapes repo", "review:\n  instructions:\n    - ../x\n"},
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
skip:
  onlyPaths:
    - "**/*.md"
review:
  instructions:
    - docs/instructions.md
  requireSuggestedFix: true
  templates:
    summary: docs/summary.tmpl
    inline: docs/inline.tmpl
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
		if !slices.Equal(f.Skip.OnlyPaths, []string{"**/*.md"}) {
			t.Fatalf("Skip.OnlyPaths = %v", f.Skip.OnlyPaths)
		}
		if !slices.Equal(f.Review.Instructions, []string{"docs/instructions.md"}) {
			t.Fatalf("Review.Instructions = %v", f.Review.Instructions)
		}
		if f.Review.RequireSuggestedFix == nil || !*f.Review.RequireSuggestedFix {
			t.Fatalf("Review.RequireSuggestedFix = %v, want true", f.Review.RequireSuggestedFix)
		}
		if f.Review.Templates.Summary != "docs/summary.tmpl" || f.Review.Templates.Inline != "docs/inline.tmpl" {
			t.Fatalf("Review.Templates = %+v", f.Review.Templates)
		}
	})
}

func TestFile_Referenced(t *testing.T) {
	t.Parallel()
	f := File{
		Review: Review{
			Instructions: []string{"docs/a.md", "docs/b.md", "docs/a.md"},
			Templates: Templates{
				Summary: "docs/a.md",
				Inline:  "docs/c.md",
			},
		},
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

	t.Run("no .kritik.yaml", func(t *testing.T) {
		t.Parallel()
		files, notes, err := Collect(mapReader(nil))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(files) != 0 {
			t.Fatalf("files = %v, want empty", files)
		}
		if notes != nil {
			t.Fatalf("notes = %v, want nil", notes)
		}
	})

	t.Run("file plus instructions and template", func(t *testing.T) {
		t.Parallel()
		doc := []byte(`review:
  instructions:
    - docs/a.md
    - docs/b.md
  templates:
    summary: docs/summary.tmpl
`)
		src := map[string][]byte{
			FileName:            doc,
			"docs/a.md":         []byte("instruction a"),
			"docs/b.md":         []byte("instruction b"),
			"docs/summary.tmpl": []byte("summary template"),
		}
		files, notes, err := Collect(mapReader(src))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(notes) != 0 {
			t.Fatalf("notes = %v, want none", notes)
		}
		want := Files{
			FileName:            string(doc),
			"docs/a.md":         "instruction a",
			"docs/b.md":         "instruction b",
			"docs/summary.tmpl": "summary template",
		}
		if !reflect.DeepEqual(files, want) {
			t.Fatalf("files = %+v, want %+v", files, want)
		}
	})

	t.Run("missing instruction noted, others kept", func(t *testing.T) {
		t.Parallel()
		doc := []byte(`review:
  instructions:
    - docs/a.md
    - docs/missing.md
`)
		src := map[string][]byte{
			FileName:    doc,
			"docs/a.md": []byte("instruction a"),
		}
		files, notes, err := Collect(mapReader(src))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if _, ok := files["docs/a.md"]; !ok {
			t.Fatalf("files = %+v, want docs/a.md kept", files)
		}
		if _, ok := files["docs/missing.md"]; ok {
			t.Fatalf("files = %+v, want docs/missing.md absent", files)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "docs/missing.md") {
			t.Fatalf("notes = %v, want one note about docs/missing.md", notes)
		}
	})

	t.Run("oversized file omitted and noted", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("a", MaxFileBytes+1)
		doc := []byte(`review:
  instructions:
    - docs/big.md
    - docs/small.md
`)
		src := map[string][]byte{
			FileName:        doc,
			"docs/big.md":   []byte(big),
			"docs/small.md": []byte("small"),
		}
		files, notes, err := Collect(mapReader(src))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if _, ok := files["docs/big.md"]; ok {
			t.Fatalf("files = %+v, want docs/big.md omitted", files)
		}
		if files["docs/small.md"] != "small" {
			t.Fatalf("files = %+v, want docs/small.md kept", files)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "docs/big.md") {
			t.Fatalf("notes = %v, want one note about docs/big.md", notes)
		}
	})

	t.Run("total budget exceeded, later files omitted", func(t *testing.T) {
		t.Parallel()
		const chunk = 220_000 // under MaxFileBytes; four of these plus the doc fit under MaxTotalBytes, a fifth and sixth do not.
		paths := []string{"docs/inst1.md", "docs/inst2.md", "docs/inst3.md", "docs/inst4.md", "docs/inst5.md", "docs/inst6.md"}
		doc := []byte(`review:
  instructions:
    - docs/inst1.md
    - docs/inst2.md
    - docs/inst3.md
    - docs/inst4.md
    - docs/inst5.md
    - docs/inst6.md
`)
		src := map[string][]byte{FileName: doc}
		for _, p := range paths {
			src[p] = []byte(strings.Repeat("a", chunk))
		}
		files, notes, err := Collect(mapReader(src))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		for _, p := range paths[:4] {
			if _, ok := files[p]; !ok {
				t.Fatalf("files missing %s, want kept", p)
			}
		}
		for _, p := range paths[4:] {
			if _, ok := files[p]; ok {
				t.Fatalf("files contains %s, want omitted", p)
			}
		}
		if len(notes) != 2 {
			t.Fatalf("notes = %v, want 2 notes about the total budget", notes)
		}
	})

	t.Run("read error other than not-exist propagates", func(t *testing.T) {
		t.Parallel()
		doc := []byte(`review:
  instructions:
    - docs/broken.md
`)
		wantErr := errors.New("disk on fire")
		read := func(p string) ([]byte, error) {
			switch p {
			case FileName:
				return doc, nil
			case "docs/broken.md":
				return nil, wantErr
			default:
				return nil, fmt.Errorf("repoconfig_test: unexpected read of %s", p)
			}
		}
		if _, _, err := Collect(read); !errors.Is(err, wantErr) {
			t.Fatalf("Collect error = %v, want it to wrap %v", err, wantErr)
		}
	})
}

func TestSkip_All(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		skip    Skip
		changed []string
		want    bool
	}{
		{"empty patterns", Skip{}, []string{"main.go"}, false},
		{"all changed paths match", Skip{OnlyPaths: []string{"**/*.md"}}, []string{"docs/a.md", "docs/b.md"}, true},
		{"one non-matching file", Skip{OnlyPaths: []string{"**/*.md"}}, []string{"docs/a.md", "main.go"}, false},
		{"empty changed", Skip{OnlyPaths: []string{"**/*.md"}}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.skip.All(c.changed); got != c.want {
				t.Fatalf("All(%v) = %v, want %v", c.changed, got, c.want)
			}
		})
	}
}

func TestCollectExtra(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		src       map[string][]byte
		extra     []string
		wantFiles []string
		wantNotes []string
	}{
		{
			name:      "extra paths are read without a .kritik.yaml",
			src:       map[string][]byte{"docs/rules.md": []byte("rules")},
			extra:     []string{"docs/rules.md", "docs/gone.md"},
			wantFiles: []string{"docs/rules.md"},
			wantNotes: []string{"docs/gone.md: referenced but not found"},
		},
		{
			name: "extra paths follow the file's own, deduplicated",
			src: map[string][]byte{
				FileName: []byte("review:\n  instructions: [docs/a.md]\n"), "docs/a.md": []byte("a"), "docs/b.md": []byte("b"),
			},
			extra:     []string{"docs/a.md", "docs/b.md"},
			wantFiles: []string{FileName, "docs/a.md", "docs/b.md"},
		},
		{
			name:      "an escaping extra path is noted, not read",
			src:       map[string][]byte{"../secret": []byte("x")},
			extra:     []string{"../secret"},
			wantNotes: []string{`repoconfig: referenced path "../secret" escapes the repository`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files, notes, err := Collect(mapReader(tt.src), tt.extra...)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			got := slices.Sorted(maps.Keys(files))
			want := slices.Sorted(slices.Values(tt.wantFiles))
			if !slices.Equal(got, want) || !slices.Equal(notes, tt.wantNotes) {
				t.Fatalf("files = %v notes = %q, want %v %q", got, notes, want, tt.wantNotes)
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
