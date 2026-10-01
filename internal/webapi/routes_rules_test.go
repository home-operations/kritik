package webapi

import (
	"reflect"
	"testing"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/store"
)

func TestCollectRules(t *testing.T) {
	settings := func(context ...configfile.ContextFile) configfile.Settings {
		var s configfile.Settings
		s.Review.Context = context
		return s
	}
	schema := configfile.ContextFile{Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}}
	account := map[string]configfile.Source{"context": configfile.SourceDefaults}
	repos := []repoRules{
		{name: "alpha/two", settings: settings(schema), sources: account},
		{
			name: "alpha/one", settings: settings(schema), sources: account,
			doc: []byte("context:\n  - path: ARCHITECTURE.md\n    description: how it fits\n"),
		},
		// A .kritika.yaml that does not parse is ignored, as a review ignores it.
		{name: "alpha/three", settings: settings(), sources: map[string]configfile.Source{}, doc: []byte("rules: {")},
	}
	// Written rules: the account's text and file rules, one alpha/one's
	// entry replaces, and a text and a file rule its .kritika.yaml adds.
	wrap := configfile.Rule{ID: "wrap-errors", Rule: "Wrap errors."}
	style := configfile.Rule{ID: "style", File: "docs/review.md"}
	own := configfile.Rule{ID: "wrap-errors", Rule: "Wrap errors here too.", Paths: []string{"**/*.go"}}
	accountRules := map[string]configfile.Scope{"wrap-errors": configfile.ScopeAccount, "style": configfile.ScopeAccount}
	repos[0].settings.Review.Rules, repos[0].ruleScopes = []configfile.Rule{wrap, style}, accountRules
	repos[1].settings.Review.Rules = []configfile.Rule{own, style}
	repos[1].ruleScopes = map[string]configfile.Scope{"wrap-errors": configfile.ScopeRepository, "style": configfile.ScopeAccount}
	repos[1].doc = append(repos[1].doc, []byte("rules:\n  - { id: wrap-errors, rule: Anything. }\n  - { id: no-tokens, rule: Never log a token. }\n"+
		"  - { id: go, file: docs/go.md, paths: ['**/*.go'], whenExpr: 'pr.baseRef == \"main\"' }\n")...)
	got := collectRules(repos)
	want := []Rule{
		{Kind: RuleContext, Path: "ARCHITECTURE.md", Description: "how it fits", Paths: []string{}, Source: RuleFromRepository, Repositories: []string{"alpha/one"}},
		{
			Kind: RuleContext, Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}, Source: "defaults",
			Repositories: []string{"alpha/one", "alpha/two"},
		},
		{Kind: RuleWritten, ID: "no-tokens", Text: "Never log a token.", Paths: []string{}, Source: RuleFromRepository, Repositories: []string{"alpha/one"}},
		{Kind: RuleWritten, ID: "wrap-errors", Text: "Wrap errors.", Paths: []string{}, Source: "account", Repositories: []string{"alpha/two"}},
		{
			Kind: RuleWritten, ID: "wrap-errors", Text: "Wrap errors here too.", Paths: []string{"**/*.go"}, Source: RuleFromEntry,
			Repositories: []string{"alpha/one"},
		},
		{
			Kind: RuleWritten, ID: "go", Path: "docs/go.md", Paths: []string{"**/*.go"}, WhenExpr: `pr.baseRef == "main"`, Source: RuleFromRepository,
			Repositories: []string{"alpha/one"},
		},
		{Kind: RuleWritten, ID: "style", Path: "docs/review.md", Paths: []string{}, Source: "account", Repositories: []string{"alpha/one", "alpha/two"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collectRules =\n%+v\nwant\n%+v", got, want)
	}
}

// TestCountCitations: a written rule counts the citations of its id in its
// own repositories only, so two rules sharing an id keep their counts
// apart; a file counts none.
func TestCountCitations(t *testing.T) {
	rules := []Rule{
		{Kind: RuleContext, Path: "wrap-errors", Repositories: []string{"alpha/one"}},
		{Kind: RuleWritten, ID: "wrap-errors", Repositories: []string{"alpha/two"}},
		{Kind: RuleWritten, ID: "wrap-errors", Repositories: []string{"alpha/one", "alpha/three"}},
		{Kind: RuleWritten, ID: "no-tokens", Repositories: []string{"alpha/one"}},
	}
	cited := []store.RuleCitation{
		{Repository: "alpha/one", Rule: "wrap-errors", Findings: 3, Addressed: 1},
		{Repository: "alpha/three", Rule: "wrap-errors", Findings: 1, Addressed: 1},
		{Repository: "alpha/two", Rule: "wrap-errors", Findings: 2, Addressed: 2},
		{Repository: "alpha/four", Rule: "no-tokens", Findings: 5},
	}
	got := countCitations(rules, cited)
	want := [][2]int{{0, 0}, {2, 2}, {4, 2}, {0, 0}}
	for i, r := range got {
		if [2]int{r.Findings, r.Addressed} != want[i] {
			t.Errorf("rule %d (%s %s) counts = %d/%d, want %d/%d", i, r.Kind, r.ID, r.Findings, r.Addressed, want[i][0], want[i][1])
		}
	}
}
