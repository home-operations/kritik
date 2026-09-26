package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
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
			Templates: configfile.ReviewTemplates{Summary: "ops/summary.tmpl", Inline: "ops/inline.tmpl"},
		},
	}
}

func TestEffective(t *testing.T) {
	operatorFiles := repoconfig.Files{"ops/rules.md": "operator rules", "ops/summary.tmpl": "op summary", "ops/inline.tmpl": "op inline"}
	with := func(extra repoconfig.Files) repoconfig.Files {
		files := maps.Clone(operatorFiles)
		maps.Copy(files, extra)
		return files
	}
	operatorDefaults := review.Templates{Summary: "op summary", Inline: "op inline"}
	operatorPaths := []string{"ops/rules.md", "ops/summary.tmpl", "ops/inline.tmpl"}

	tests := []struct {
		name string
		// doc is the merge-base .kritik.yaml, none when empty; files are
		// what the runner read, and runnerNotes what it noted.
		doc          string
		files        repoconfig.Files
		runnerNotes  []string
		enabled      bool
		inRepoFilter bool
		ignore       []string
		repoFiles    []string
		instructions []string
		templates    review.Templates
		strict       bool
		onlyPaths    []string
		notes        []string
	}{
		{
			name: "no file keeps the operator's settings", files: operatorFiles, enabled: true, ignore: []string{"vendor/**"},
			repoFiles: operatorPaths, instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "disable", doc: "enabled: false\n", files: operatorFiles, ignore: []string{"vendor/**"},
			repoFiles: append(operatorPaths, repoconfig.FileName), instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "filter is kept apart to be ANDed", doc: "filter: '!pr.body.contains(\"[skip-review]\")'\n", files: operatorFiles,
			enabled: true, inRepoFilter: true, ignore: []string{"vendor/**"}, repoFiles: append(operatorPaths, repoconfig.FileName),
			instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "ignore and skip paths add to the operator's", doc: "ignore: [gen/**, vendor/**]\nskip:\n  onlyPaths: [docs/**]\n",
			files: operatorFiles, enabled: true, ignore: []string{"vendor/**", "gen/**"}, onlyPaths: []string{"docs/**"},
			repoFiles: append(operatorPaths, repoconfig.FileName), instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
		},
		{
			name: "repository strictness wins over the operator's", doc: "review:\n  requireSuggestedFix: false\n", files: operatorFiles,
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(operatorPaths, repoconfig.FileName),
			instructions: []string{"operator rules"}, templates: operatorDefaults,
		},
		{
			name:    "repository instructions and summary template replace the operator's",
			doc:     "review:\n  instructions: [.kritik/rules.md]\n  templates:\n    summary: .kritik/summary.tmpl\n",
			files:   with(repoconfig.Files{".kritik/rules.md": "repo rules", ".kritik/summary.tmpl": "repo summary"}),
			enabled: true, ignore: []string{"vendor/**"},
			repoFiles:    []string{".kritik/rules.md", ".kritik/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			instructions: []string{"repo rules"}, templates: review.Templates{Summary: "repo summary", Inline: "op inline"}, strict: true,
		},
		{
			name: "a missing instruction file is noted", doc: "review:\n  instructions: [.kritik/rules.md, .kritik/gone.md]\n",
			files: with(repoconfig.Files{".kritik/rules.md": "repo rules"}), enabled: true, ignore: []string{"vendor/**"},
			repoFiles:    []string{".kritik/rules.md", ".kritik/gone.md", "ops/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			instructions: []string{"repo rules"}, templates: operatorDefaults, strict: true,
			notes: []string{".kritik/gone.md: referenced but not found"},
		},
		{
			name: "a file the runner noted is not noted again", doc: "review:\n  instructions: [.kritik/big.md, .kritik/gone.md]\n",
			files: operatorFiles,
			runnerNotes: []string{
				".kritik/big.md: skipped, it exceeds the 262144 byte per-file limit", ".kritik/gone.md: referenced but not found",
			},
			enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{".kritik/big.md", ".kritik/gone.md", "ops/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			templates: operatorDefaults, strict: true,
			notes: []string{
				".kritik/big.md: skipped, it exceeds the 262144 byte per-file limit", ".kritik/gone.md: referenced but not found",
			},
		},
		{
			name: "instructions are capped at a UTF-8 boundary", doc: "review:\n  instructions: [.kritik/a.md, .kritik/b.md]\n",
			files:   with(repoconfig.Files{".kritik/a.md": strings.Repeat("a", repoconfig.MaxInstructionBytes-1) + "é", ".kritik/b.md": "never seen"}),
			enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{".kritik/a.md", ".kritik/b.md", "ops/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			templates: operatorDefaults, strict: true, instructions: []string{strings.Repeat("a", repoconfig.MaxInstructionBytes-1)},
			notes: []string{"repository instructions truncated to 32 KiB"},
		},
		{
			name: "invalid yaml is noted and the operator's settings apply", doc: "enabled: false\nunknown: 1\n", files: operatorFiles,
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(operatorPaths, repoconfig.FileName),
			instructions: []string{"operator rules"}, templates: operatorDefaults, strict: true,
			notes: []string{".kritik.yaml was ignored: repoconfig: parse: yaml: unmarshal errors:\n  line 2: field unknown not found in type repoconfig.File"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := operatorSettings(t)
			var doc []byte
			if tt.doc != "" {
				doc = []byte(tt.doc)
			}
			e, notes := effective(settings, doc)
			if e.Enabled != tt.enabled || (e.InRepoFilter != nil) != tt.inRepoFilter || e.Filter != settings.Filter {
				t.Fatalf("enabled=%v inRepoFilter=%v operator filter kept=%v", e.Enabled, e.InRepoFilter != nil, e.Filter == settings.Filter)
			}
			if !slices.Equal(e.Ignore, tt.ignore) || !slices.Equal(e.Skip.OnlyPaths, tt.onlyPaths) {
				t.Fatalf("ignore=%v onlyPaths=%v", e.Ignore, e.Skip.OnlyPaths)
			}
			if got := e.repoFiles(); !slices.Equal(got, tt.repoFiles) {
				t.Fatalf("repoFiles = %v, want %v", got, tt.repoFiles)
			}
			notes = e.fill(tt.files, append(notes, tt.runnerNotes...))
			if !slices.Equal(e.Instructions, tt.instructions) || e.Templates != tt.templates || e.Review.RequireSuggestedFix != tt.strict {
				t.Fatalf("instructions=%q templates=%+v strict=%v", e.Instructions, e.Templates, e.Review.RequireSuggestedFix)
			}
			if !slices.Equal(notes, tt.notes) {
				t.Fatalf("notes = %q, want %q", notes, tt.notes)
			}
			if !slices.Equal(settings.Ignore, []string{"vendor/**"}) || !slices.Equal(settings.Review.Instructions, []string{"ops/rules.md"}) {
				t.Fatalf("the operator's settings were modified: %v %v", settings.Ignore, settings.Review.Instructions)
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
			e, _ := effective(configfile.Settings{Enabled: true}, []byte(tt.doc))
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

// fileForge is a forge whose only call is FileAt, answering from files, or
// with err when set.
type fileForge struct {
	forge.Client
	files map[string]string
	err   error
}

func (f fileForge) FileAt(_ context.Context, _, _, _, path string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	content, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", path, fs.ErrNotExist)
	}
	return []byte(content), nil
}

func TestReadRepoConfig(t *testing.T) {
	down := errors.New("forge down")
	tests := []struct {
		name    string
		client  fileForge
		doc     string
		notes   []string
		wantErr error
	}{
		{name: "none", client: fileForge{}},
		{name: "read", client: fileForge{files: map[string]string{repoconfig.FileName: "enabled: false\n"}}, doc: "enabled: false\n"},
		{
			name:   "over the file cap",
			client: fileForge{files: map[string]string{repoconfig.FileName: strings.Repeat("x", repoconfig.MaxFileBytes+1)}},
			notes:  []string{repoconfig.TooLarge(repoconfig.FileName)},
		},
		{
			name:   "over what the forge reads",
			client: fileForge{err: fmt.Errorf("fake: %w", forge.ErrFileTooLarge)},
			notes:  []string{repoconfig.TooLarge(repoconfig.FileName)},
		},
		{name: "a forge error fails the job for a retry", client: fileForge{err: down}, wantErr: down},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, notes, err := readRepoConfig(t.Context(), tt.client, "o", "r", "base")
			if !errors.Is(err, tt.wantErr) || string(doc) != tt.doc || (doc == nil) != (tt.doc == "") || !slices.Equal(notes, tt.notes) {
				t.Fatalf("readRepoConfig = %q, %q, %v", doc, notes, err)
			}
		})
	}
}

func TestSettleLeft(t *testing.T) {
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		trigger string
		settle  time.Duration
		now     time.Time
		want    time.Duration
	}{
		{"a push waits out the settle time", "synchronize", time.Minute, created.Add(20 * time.Second), 40 * time.Second},
		{"so does a head the poller found", "poll", time.Minute, created, time.Minute},
		{"a push past its settle time runs", "synchronize", time.Minute, created.Add(2 * time.Minute), -time.Minute},
		{"no settle time", "synchronize", 0, created, 0},
		{"an opened pull request does not wait", "opened", time.Minute, created, 0},
		{"nor does a manual re-run", "manual", time.Minute, created, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := settleLeft(tt.trigger, tt.settle, created, tt.now); got != tt.want {
				t.Fatalf("settleLeft = %v, want %v", got, tt.want)
			}
		})
	}
}
