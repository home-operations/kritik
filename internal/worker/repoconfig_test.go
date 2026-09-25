package worker

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/prfilter"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

func operatorSettings(t *testing.T) configfile.Settings {
	t.Helper()
	filter, err := prfilter.Compile("!pr.draft")
	if err != nil {
		t.Fatal(err)
	}
	return configfile.Settings{
		Enabled: true, Filter: filter, Ignore: []string{"vendor/**"},
		Review: configfile.Review{
			Instructions: []string{"ops/rules.md"}, RequireSuggestedFix: true,
			Templates: configfile.ReviewTemplates{Summary: "ops/summary.j2", Inline: "ops/inline.j2"},
		},
	}
}

func TestEffective(t *testing.T) {
	operatorFiles := repoconfig.Files{"ops/rules.md": "operator rules", "ops/summary.j2": "op summary", "ops/inline.j2": "op inline"}
	withFile := func(doc string, extra repoconfig.Files) repoconfig.Files {
		files := repoconfig.Files{repoconfig.FileName: doc}
		for _, src := range []repoconfig.Files{operatorFiles, extra} {
			maps.Copy(files, src)
		}
		return files
	}
	operatorDefaults := review.Templates{Summary: "op summary", Inline: "op inline"}

	tests := []struct {
		name         string
		files        repoconfig.Files
		enabled      bool
		inRepoFilter bool
		ignore       []string
		instructions []string
		templates    review.Templates
		strict       bool
		onlyPaths    []string
		runnerNotes  []string
		notes        []string
	}{
		{
			name: "no file keeps the operator's settings", files: operatorFiles, enabled: true, ignore: []string{"vendor/**"},
			instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "disable", files: withFile("enabled: false\n", nil), ignore: []string{"vendor/**"},
			instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "filter is kept apart to be ANDed", files: withFile("filter: '!pr.body.contains(\"[skip-review]\")'\n", nil),
			enabled: true, inRepoFilter: true, ignore: []string{"vendor/**"},
			instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "ignore and skip paths add to the operator's", files: withFile("ignore: [gen/**, vendor/**]\nskip:\n  onlyPaths: [docs/**]\n", nil),
			enabled: true, ignore: []string{"vendor/**", "gen/**"}, onlyPaths: []string{"docs/**"},
			instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "repository strictness wins over the operator's", files: withFile("review:\n  requireSuggestedFix: false\n", nil),
			enabled: true, ignore: []string{"vendor/**"}, instructions: []string{"operator rules"}, templates: operatorDefaults,
		},
		{
			name: "repository instructions and summary template replace the operator's",
			files: withFile("review:\n  instructions: [.kritik/rules.md]\n  templates:\n    summary: .kritik/summary.j2\n",
				repoconfig.Files{".kritik/rules.md": "repo rules", ".kritik/summary.j2": "repo summary"}),
			enabled: true, ignore: []string{"vendor/**"}, instructions: []string{"repo rules"},
			templates: review.Templates{Summary: "repo summary", Inline: "op inline"}, strict: true,
		},
		{
			name:    "a missing instruction file is noted",
			files:   withFile("review:\n  instructions: [.kritik/rules.md, .kritik/gone.md]\n", repoconfig.Files{".kritik/rules.md": "repo rules"}),
			enabled: true, ignore: []string{"vendor/**"}, instructions: []string{"repo rules"}, templates: operatorDefaults, strict: true,
			notes: []string{".kritik/gone.md: referenced but not found"},
		},
		{
			name:  "a file the runner noted is not noted again",
			files: withFile("review:\n  instructions: [.kritik/big.md, .kritik/gone.md]\n", nil),
			runnerNotes: []string{
				".kritik/big.md: skipped, it exceeds the 262144 byte per-file limit", ".kritik/gone.md: referenced but not found",
			},
			enabled: true, ignore: []string{"vendor/**"}, templates: operatorDefaults, strict: true,
			notes: []string{
				".kritik/big.md: skipped, it exceeds the 262144 byte per-file limit", ".kritik/gone.md: referenced but not found",
			},
		},
		{
			name: "instructions are capped at a UTF-8 boundary",
			files: withFile("review:\n  instructions: [.kritik/a.md, .kritik/b.md]\n",
				repoconfig.Files{".kritik/a.md": strings.Repeat("a", repoconfig.MaxInstructionBytes-1) + "é", ".kritik/b.md": "never seen"}),
			enabled: true, ignore: []string{"vendor/**"}, templates: operatorDefaults, strict: true,
			instructions: []string{strings.Repeat("a", repoconfig.MaxInstructionBytes-1)},
			notes:        []string{"repository instructions truncated to 32 KiB"},
		},
		{
			name: "invalid yaml is noted and the operator's settings apply", files: withFile("enabled: false\nunknown: 1\n", nil),
			enabled: true, ignore: []string{"vendor/**"}, instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
			notes: []string{".kritik.yaml was ignored: repoconfig: parse: yaml: unmarshal errors:\n  line 2: field unknown not found in type repoconfig.File"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := operatorSettings(t)
			e, notes := effective(settings, tt.files, tt.runnerNotes)
			if e.Enabled != tt.enabled || (e.InRepoFilter != nil) != tt.inRepoFilter || e.Filter != settings.Filter {
				t.Fatalf("enabled=%v inRepoFilter=%v operator filter kept=%v", e.Enabled, e.InRepoFilter != nil, e.Filter == settings.Filter)
			}
			if !slices.Equal(e.Ignore, tt.ignore) || !slices.Equal(e.Skip.OnlyPaths, tt.onlyPaths) {
				t.Fatalf("ignore=%v onlyPaths=%v", e.Ignore, e.Skip.OnlyPaths)
			}
			if !slices.Equal(e.Instructions, tt.instructions) || e.Templates != tt.templates || e.RequireSuggestedFix != tt.strict {
				t.Fatalf("instructions=%q templates=%+v strict=%v", e.Instructions, e.Templates, e.RequireSuggestedFix)
			}
			if !slices.Equal(notes, tt.notes) {
				t.Fatalf("notes = %q, want %q", notes, tt.notes)
			}
			if !slices.Equal(settings.Ignore, []string{"vendor/**"}) {
				t.Fatalf("the operator's settings were modified: %v", settings.Ignore)
			}
		})
	}
}

func TestEffectiveSkip(t *testing.T) {
	vars := func(body string) map[string]any {
		return map[string]any{"title": "t", "body": body, "draft": false, "labels": []any{}}
	}
	tests := []struct {
		name    string
		doc     string
		body    string
		changed []string
		want    repoconfig.SkipReason
	}{
		{"nothing to skip", "", "", []string{"main.go"}, ""},
		{"disabled", "enabled: false\n", "", []string{"main.go"}, repoconfig.SkipDisabled},
		{"filtered", "filter: '!pr.body.contains(\"[skip-review]\")'\n", "please [skip-review]", []string{"main.go"}, repoconfig.SkipFiltered},
		{"filter allows", "filter: '!pr.body.contains(\"[skip-review]\")'\n", "normal", []string{"main.go"}, ""},
		{"filter that fails to evaluate skips", "filter: 'pr.number > 0'\n", "", []string{"main.go"}, repoconfig.SkipFiltered},
		{"only skipped paths", "skip:\n  onlyPaths: [docs/**]\n", "", []string{"docs/a.md", "docs/b/c.md"}, repoconfig.SkipOnlyPaths},
		{"a path outside the skip rule", "skip:\n  onlyPaths: [docs/**]\n", "", []string{"docs/a.md", "main.go"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := effective(configfile.Settings{Enabled: true}, repoconfig.Files{repoconfig.FileName: tt.doc}, nil)
			got, _ := e.skip(vars(tt.body), tt.changed)
			if got != tt.want {
				t.Fatalf("skip = %q, want %q", got, tt.want)
			}
			if got != "" && !got.Valid() {
				t.Fatalf("reason %q is not valid", got)
			}
		})
	}
	for r, want := range map[repoconfig.SkipReason]string{
		repoconfig.SkipDisabled: "disabled in .kritik.yaml", repoconfig.SkipFiltered: "filtered by .kritik.yaml", repoconfig.SkipOnlyPaths: "only skipped paths changed",
	} {
		if r.Description() != want {
			t.Fatalf("%q.Description() = %q, want %q", r, r.Description(), want)
		}
	}
	if repoconfig.SkipReason("other").Valid() {
		t.Fatal("an unknown reason must not be valid")
	}
}
