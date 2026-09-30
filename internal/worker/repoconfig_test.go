package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
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

func adminSettings(t *testing.T) configfile.Settings {
	t.Helper()
	filter, err := prfilter.Compile("!pr.draft")
	if err != nil {
		t.Fatal(err)
	}
	return configfile.Settings{
		Enabled: true, Filter: filter, Ignore: []string{"vendor/**"},
		Review: configfile.Review{
			Rules: []configfile.Rule{{ID: "ops", File: "ops/rules.md"}}, RequireSuggestedFix: true,
			Templates: configfile.ReviewTemplates{Summary: "ops/summary.tmpl", Inline: "ops/inline.tmpl"},
		},
	}
}

func TestEffective(t *testing.T) {
	adminFiles := repoconfig.Files{"ops/rules.md": "admin rules", "ops/summary.tmpl": "op summary", "ops/inline.tmpl": "op inline"}
	with := func(extra repoconfig.Files) repoconfig.Files {
		files := maps.Clone(adminFiles)
		maps.Copy(files, extra)
		return files
	}
	adminDefaults := review.Templates{Summary: "op summary", Inline: "op inline"}
	adminPaths := []string{"ops/rules.md", "ops/summary.tmpl", "ops/inline.tmpl"}
	adminRules := []review.Rule{{ID: "ops", Text: "admin rules", File: "ops/rules.md"}}
	repoRule := review.Rule{ID: "repo", Text: "repo rules", File: ".kritik/rules.md"}

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
		rules        []review.Rule
		instructions []string
		templates    review.Templates
		strict       bool
		onlyPaths    []string
		notes        []string
	}{
		{
			name: "no file keeps the admin's settings", files: adminFiles, enabled: true, ignore: []string{"vendor/**"},
			repoFiles: adminPaths, rules: adminRules, templates: adminDefaults, strict: true,
		},
		{
			name: "disable", doc: "enabled: false\n", files: adminFiles, ignore: []string{"vendor/**"},
			repoFiles: append(adminPaths, repoconfig.FileName), rules: adminRules, templates: adminDefaults, strict: true,
		},
		{
			name: "filter is kept apart to be ANDed", doc: "filter: '!pr.body.contains(\"[skip-review]\")'\n", files: adminFiles,
			enabled: true, inRepoFilter: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			rules: adminRules, templates: adminDefaults, strict: true,
		},
		{
			name: "ignore and skip paths add to the admin's", doc: "ignore: [gen/**, vendor/**]\nskip:\n  onlyPaths: [docs/**]\n",
			files: adminFiles, enabled: true, ignore: []string{"vendor/**", "gen/**"}, onlyPaths: []string{"docs/**"},
			repoFiles: append(adminPaths, repoconfig.FileName), rules: adminRules, templates: adminDefaults, strict: true,
		},
		{
			name: "requireSuggestedFix may only turn on", doc: "review:\n  requireSuggestedFix: false\n", files: adminFiles,
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			rules: adminRules, templates: adminDefaults, strict: true,
			notes: []string{".kritik.yaml: review.requireSuggestedFix false was dropped; allowed: true, since an admin requires a suggested fix"},
		},
		{
			name:    "repository file rules follow the admin's, and its summary template replaces the admin's",
			doc:     "review:\n  rules: [{ id: repo, file: .kritik/rules.md }]\n  templates:\n    summary: .kritik/summary.tmpl\n",
			files:   with(repoconfig.Files{".kritik/rules.md": "repo rules", ".kritik/summary.tmpl": "repo summary"}),
			enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{"ops/rules.md", ".kritik/rules.md", ".kritik/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			rules:     append(slices.Clone(adminRules), repoRule), templates: review.Templates{Summary: "repo summary", Inline: "op inline"}, strict: true,
		},
		{
			name: "a missing rule file is noted", doc: "review:\n  rules: [{ id: repo, file: .kritik/rules.md }, { id: gone, file: .kritik/gone.md }]\n",
			files: with(repoconfig.Files{".kritik/rules.md": "repo rules"}), enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{"ops/rules.md", ".kritik/rules.md", ".kritik/gone.md", "ops/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			rules:     append(slices.Clone(adminRules), repoRule), templates: adminDefaults, strict: true,
			notes: []string{".kritik/gone.md: referenced but not found"},
		},
		{
			name: "a file the runner noted is not noted again", doc: "review:\n  rules: [{ id: big, file: .kritik/big.md }, { id: gone, file: .kritik/gone.md }]\n",
			files: adminFiles,
			runnerNotes: []string{
				".kritik/big.md: skipped, it exceeds the 262144 byte per-file limit", ".kritik/gone.md: referenced but not found",
			},
			enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{"ops/rules.md", ".kritik/big.md", ".kritik/gone.md", "ops/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			rules:     adminRules, templates: adminDefaults, strict: true,
			notes: []string{
				".kritik/big.md: skipped, it exceeds the 262144 byte per-file limit", ".kritik/gone.md: referenced but not found",
			},
		},
		{
			// The file is one byte too long by its last character, whose
			// first byte would still fit.
			name: "agent files are capped at a UTF-8 boundary", doc: "review: { agentFiles: true }\n",
			files:   with(repoconfig.Files{"AGENTS.md": strings.Repeat("a", repoconfig.MaxInstructionBytes-1) + "é"}),
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			rules: adminRules, templates: adminDefaults, strict: true,
			instructions: []string{strings.Repeat("a", repoconfig.MaxInstructionBytes-1)},
			notes:        []string{"AGENTS.md and CLAUDE.md files truncated to 32 KiB"},
		},
		{
			name: "invalid yaml is noted and the admin's settings apply", doc: "enabled: false\nunknown: 1\n", files: adminFiles,
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			rules: adminRules, templates: adminDefaults, strict: true,
			notes: []string{".kritik.yaml was ignored: repoconfig: parse: yaml: unmarshal errors:\n  line 2: field unknown not found in type repoconfig.File"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := adminSettings(t)
			var doc []byte
			if tt.doc != "" {
				doc = []byte(tt.doc)
			}
			e, notes := effective(settings, doc)
			if e.Enabled != tt.enabled || (e.InRepoFilter != nil) != tt.inRepoFilter || e.Filter != settings.Filter {
				t.Fatalf("enabled=%v inRepoFilter=%v admin filter kept=%v", e.Enabled, e.InRepoFilter != nil, e.Filter == settings.Filter)
			}
			if !slices.Equal(e.Ignore, tt.ignore) || !slices.Equal(e.Skip.OnlyPaths, tt.onlyPaths) {
				t.Fatalf("ignore=%v onlyPaths=%v", e.Ignore, e.Skip.OnlyPaths)
			}
			if got := e.repoFiles(); !slices.Equal(got, tt.repoFiles) {
				t.Fatalf("repoFiles = %v, want %v", got, tt.repoFiles)
			}
			notes = e.fill(tt.files, append(notes, tt.runnerNotes...), []string{"main.go"})
			if !reflect.DeepEqual(e.Rules, tt.rules) || !slices.Equal(e.Instructions, tt.instructions) || e.Templates != tt.templates ||
				e.Review.RequireSuggestedFix != tt.strict {
				t.Fatalf("rules=%+v instructions=%.40q templates=%+v strict=%v", e.Rules, e.Instructions, e.Templates, e.Review.RequireSuggestedFix)
			}
			if !slices.Equal(notes, tt.notes) {
				t.Fatalf("notes = %q, want %q", notes, tt.notes)
			}
			if !slices.Equal(settings.Ignore, []string{"vendor/**"}) || !reflect.DeepEqual(settings.Review.Rules, adminSettings(t).Review.Rules) {
				t.Fatalf("the admin's settings were modified: %v %v", settings.Ignore, settings.Review.Rules)
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
			got, _ := e.Check(vars(tt.body), tt.changed)
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

func (f fileForge) MergeBase(context.Context, string, string, string, string) (string, error) {
	return "base", nil
}

func TestFollowUpRepoConfig(t *testing.T) {
	files := map[string]string{"ops/rules.md": "admin rules", ".kritik/rules.md": "repo rules"}
	with := func(doc string) map[string]string {
		m := maps.Clone(files)
		m[repoconfig.FileName] = doc
		return m
	}
	tests := []struct {
		name   string
		files  map[string]string
		reason string
		model  configfile.ModelRef
		rules  []string
	}{
		{name: "no file", files: files, model: "p/big", rules: []string{"admin rules"}},
		{
			name: "the repository's model and file rules", files: with("models: { review: p/small }\nreview: { rules: [{ id: repo, file: .kritik/rules.md }] }\n"),
			model: "p/small", rules: []string{"admin rules", "repo rules"},
		},
		{name: "a model outside the bounds is dropped", files: with("models: { review: p/huge }\n"), model: "p/big", rules: []string{"admin rules"}},
		{name: "disabled", files: with("enabled: false\n"), reason: "disabled in .kritik.yaml", model: "p/big"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := adminSettings(t)
			settings.Models.Review = "p/big"
			settings.Allow.Models = []configfile.ModelRef{"p/big", "p/small"}
			f := &followUp{client: fileForge{files: tt.files}, owner: "o", repo: "r", pr: &pullRequest{number: 1}, settings: settings}
			reason, err := f.repoConfig(t.Context())
			if err != nil || reason != tt.reason {
				t.Fatalf("repoConfig = %q, %v; want %q", reason, err, tt.reason)
			}
			active, _ := repoconfig.ActiveRules(f.settings.Review.Rules, f.ruleFiles, nil)
			var rules []string
			for _, r := range active {
				rules = append(rules, r.Text)
			}
			if f.settings.Models.Review != tt.model || !slices.Equal(rules, tt.rules) {
				t.Fatalf("model = %s, rules = %q", f.settings.Models.Review, rules)
			}
		})
	}
}

func TestPostsInline(t *testing.T) {
	tests := []struct {
		name   string
		review configfile.Review
		want   map[review.Severity]bool
	}{
		{"every finding, detailed", configfile.Review{InlineComments: true, Feedback: configfile.FeedbackDetailed},
			map[review.Severity]bool{review.SeverityBlocking: true, review.SeverityImportant: true, review.SeverityNit: true}},
		{"every finding, minimal", configfile.Review{InlineComments: true, Feedback: configfile.FeedbackMinimal},
			map[review.Severity]bool{review.SeverityBlocking: true, review.SeverityImportant: true, review.SeverityNit: true}},
		{"nits to the summary, standard", configfile.Review{InlineComments: true, Feedback: configfile.FeedbackStandard},
			map[review.Severity]bool{review.SeverityBlocking: true, review.SeverityImportant: true, review.SeverityNit: false}},
		{"none with inline comments off", configfile.Review{Feedback: configfile.FeedbackDetailed},
			map[review.Severity]bool{review.SeverityBlocking: false, review.SeverityImportant: false, review.SeverityNit: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &publishPhase{settings: configfile.Settings{Review: tt.review}}
			for sev, want := range tt.want {
				if got := p.postsInline(review.Finding{Severity: sev}); got != want {
					t.Errorf("postsInline(%s) = %v, want %v", sev, got, want)
				}
			}
		})
	}
}

func TestFillScopesRules(t *testing.T) {
	e, _ := effective(adminSettings(t), []byte("review:\n  rules: [{ id: sql, file: .kritik/sql.md, paths: ['**/*.sql'] }]\n"))
	files := repoconfig.Files{"ops/rules.md": "admin rules", ".kritik/sql.md": "sql rules"}
	for _, tt := range []struct {
		changed []string
		want    []string
	}{
		{[]string{"main.go"}, []string{"ops"}},
		{[]string{"main.go", "db/0001.sql"}, []string{"ops", "sql"}},
	} {
		e.fill(files, nil, tt.changed)
		got := make([]string, 0, len(e.Rules))
		for _, r := range e.Rules {
			got = append(got, r.ID)
		}
		if !slices.Equal(got, tt.want) {
			t.Fatalf("changed %v: rules = %q, want %q", tt.changed, got, tt.want)
		}
	}
}

func TestFillReferences(t *testing.T) {
	settings := adminSettings(t)
	settings.Review.Context = []configfile.ContextFile{{Path: "docs/arch.md", Description: "how the parts fit"}}
	e, _ := effective(settings, []byte("review:\n  context: [{ path: db/schema.sql, description: the schema, paths: ['**/*.sql'] }, "+
		"{ path: docs/gone.md, description: gone }]\n"))
	files := repoconfig.Files{
		"ops/rules.md": "admin rules", "ops/summary.tmpl": "s", "ops/inline.tmpl": "i", "docs/arch.md": "arch", "db/schema.sql": "schema",
	}
	notes := e.fill(files, nil, []string{"main.go"})
	want := []review.Reference{{Path: "docs/arch.md", Description: "how the parts fit", Content: "arch"}}
	if !reflect.DeepEqual(e.References, want) || !slices.Equal(notes, []string{"docs/gone.md: referenced but not found"}) {
		t.Fatalf("references = %+v, notes = %q", e.References, notes)
	}
	e.fill(files, nil, []string{"db/0002.sql"})
	if len(e.References) != 2 || e.References[1].Content != "schema" {
		t.Fatalf("references = %+v, want the schema too", e.References)
	}
}
