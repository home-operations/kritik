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

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/prfilter"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
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

	tests := []struct {
		name string
		// doc is the merge-base .kritika.yaml, none when empty; files are
		// what the runner read.
		doc          string
		files        repoconfig.Files
		enabled      bool
		inRepoFilter bool
		ignore       []string
		repoFiles    []string
		templates    review.Templates
		strict       bool
		notes        []string
	}{
		{
			name: "no file keeps the admin's settings", files: adminFiles, enabled: true, ignore: []string{"vendor/**"},
			repoFiles: adminPaths, templates: adminDefaults, strict: true,
		},
		{
			name: "disable", doc: "enabled: false\n", files: adminFiles, ignore: []string{"vendor/**"},
			repoFiles: append(adminPaths, repoconfig.FileName), templates: adminDefaults, strict: true,
		},
		{
			name: "filter is kept apart to be ANDed", doc: "filterExpr: '!pr.body.contains(\"[skip-review]\")'\n", files: adminFiles,
			enabled: true, inRepoFilter: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			templates: adminDefaults, strict: true,
		},
		{
			name: "ignore globs add to the admin's", doc: "ignore: [gen/**, vendor/**]\n",
			files: adminFiles, enabled: true, ignore: []string{"vendor/**", "gen/**"},
			repoFiles: append(adminPaths, repoconfig.FileName), templates: adminDefaults, strict: true,
		},
		{
			name: "requireSuggestedFix may only turn on", doc: "requireSuggestedFix: false\n", files: adminFiles,
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			templates: adminDefaults, strict: true,
			notes: []string{".kritika.yaml: requireSuggestedFix false was dropped; allowed: true, since an admin requires a suggested fix"},
		},
		{
			name:    "repository file rules follow the admin's, and its summary template replaces the admin's",
			doc:     "rules: [{ id: repo, file: .kritika/rules.md }]\ncomments:\n  summaryTemplate: .kritika/summary.tmpl\n",
			files:   with(repoconfig.Files{".kritika/rules.md": "repo rules", ".kritika/summary.tmpl": "repo summary"}),
			enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{"ops/rules.md", ".kritika/rules.md", ".kritika/summary.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			templates: review.Templates{Summary: "repo summary", Inline: "op inline"}, strict: true,
		},
		{
			name: "a template the runner could not read leaves the built-in one", doc: "comments:\n  summaryTemplate: .kritika/gone.tmpl\n",
			files: adminFiles, enabled: true, ignore: []string{"vendor/**"},
			repoFiles: []string{"ops/rules.md", ".kritika/gone.tmpl", "ops/inline.tmpl", repoconfig.FileName},
			templates: review.Templates{Inline: "op inline"}, strict: true,
		},
		{
			name: "invalid yaml is noted and the admin's settings apply", doc: "enabled: false\nunknown: 1\n", files: adminFiles,
			enabled: true, ignore: []string{"vendor/**"}, repoFiles: append(adminPaths, repoconfig.FileName),
			templates: adminDefaults, strict: true,
			notes: []string{".kritika.yaml was ignored: repoconfig: parse: yaml: unmarshal errors:\n  line 2: field unknown not found in type repoconfig.File"},
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
			if !slices.Equal(e.Ignore, tt.ignore) {
				t.Fatalf("ignore=%v", e.Ignore)
			}
			if got := e.repoFiles(); !slices.Equal(got, tt.repoFiles) {
				t.Fatalf("repoFiles = %v, want %v", got, tt.repoFiles)
			}
			if got := e.templates(tt.files); got != tt.templates || e.Review.RequireSuggestedFix != tt.strict {
				t.Fatalf("templates=%+v strict=%v", got, e.Review.RequireSuggestedFix)
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
		{"filtered", "filterExpr: '!pr.body.contains(\"[skip-review]\")'\n", "please [skip-review]", []string{"main.go"}, repoconfig.SkipFiltered},
		{"filter allows", "filterExpr: '!pr.body.contains(\"[skip-review]\")'\n", "normal", []string{"main.go"}, ""},
		{"filter that fails to evaluate skips", "filterExpr: 'pr.number > 0'\n", "", []string{"main.go"}, repoconfig.SkipFiltered},
		{"only ignored paths", "ignore: [docs/**]\n", "", []string{"docs/a.md", "docs/b/c.md"}, repoconfig.SkipOnlyPaths},
		{"a path outside the ignore globs", "ignore: [docs/**]\n", "", []string{"docs/a.md", "main.go"}, ""},
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
		repoconfig.SkipDisabled: "disabled in .kritika.yaml", repoconfig.SkipFiltered: "filtered by .kritika.yaml", repoconfig.SkipOnlyPaths: "only ignored paths changed",
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
	files := map[string]string{"ops/rules.md": "admin rules", ".kritika/rules.md": "repo rules"}
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
			name: "the repository's model and file rules", files: with("models: { review: p/small }\nrules: [{ id: repo, file: .kritika/rules.md }]\n"),
			model: "p/small", rules: []string{"admin rules", "repo rules"},
		},
		{name: "a model of a provider the account may not use is dropped", files: with("models: { review: q/huge }\n"), model: "p/big", rules: []string{"admin rules"}},
		{name: "disabled", files: with("enabled: false\n"), reason: "disabled in .kritika.yaml", model: "p/big"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := adminSettings(t)
			settings.Models.Review = "p/big"
			settings.Providers = []string{"p"}
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
