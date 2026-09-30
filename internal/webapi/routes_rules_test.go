package webapi

import (
	"reflect"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestCollectRules(t *testing.T) {
	settings := func(instructions []string, context ...configfile.ContextFile) configfile.Settings {
		var s configfile.Settings
		s.Review.Instructions, s.Review.Context = instructions, context
		return s
	}
	schema := configfile.ContextFile{Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}}
	account := map[string]configfile.Source{"review.instructions": configfile.SourceAccount, "review.context": configfile.SourceDefaults}
	repos := []repoRules{
		{name: "alpha/two", settings: settings([]string{"docs/review.md"}, schema), sources: account},
		{
			name: "alpha/one", settings: settings([]string{"docs/review.md"}, schema), sources: account,
			doc: []byte("review:\n  instructions:\n    - docs/review.md\n    - path: docs/go.md\n      paths: ['**/*.go']\n" +
				"  context:\n    - path: ARCHITECTURE.md\n      description: how it fits\n"),
		},
		// A .kritik.yaml that does not parse is ignored, as a review ignores it.
		{name: "alpha/three", settings: settings(nil), sources: map[string]configfile.Source{}, doc: []byte("review: [")},
	}
	// Written rules: the account's, one alpha/one's entry replaces, and
	// one its .kritik.yaml adds.
	wrap := configfile.Rule{ID: "wrap-errors", Rule: "Wrap errors."}
	own := configfile.Rule{ID: "wrap-errors", Rule: "Wrap errors here too.", Paths: []string{"**/*.go"}}
	repos[0].settings.Review.Rules, repos[0].ruleScopes = []configfile.Rule{wrap}, map[string]configfile.Scope{"wrap-errors": configfile.ScopeAccount}
	repos[1].settings.Review.Rules, repos[1].ruleScopes = []configfile.Rule{own}, map[string]configfile.Scope{"wrap-errors": configfile.ScopeRepository}
	repos[1].doc = append(repos[1].doc, []byte("  rules:\n    - { id: wrap-errors, rule: Anything. }\n    - { id: no-tokens, rule: Never log a token. }\n")...)
	got := collectRules(repos)
	want := []Rule{
		{Kind: RuleContext, Path: "ARCHITECTURE.md", Description: "how it fits", Paths: []string{}, Source: RuleFromRepository, Repositories: []string{"alpha/one"}},
		{
			Kind: RuleContext, Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}, Source: "defaults",
			Repositories: []string{"alpha/one", "alpha/two"},
		},
		{Kind: RuleInstructions, Path: "docs/go.md", Paths: []string{"**/*.go"}, Source: RuleFromRepository, Repositories: []string{"alpha/one"}},
		{Kind: RuleInstructions, Path: "docs/review.md", Paths: []string{}, Source: "account", Repositories: []string{"alpha/one", "alpha/two"}},
		{Kind: RuleWritten, ID: "no-tokens", Text: "Never log a token.", Paths: []string{}, Source: RuleFromRepository, Repositories: []string{"alpha/one"}},
		{Kind: RuleWritten, ID: "wrap-errors", Text: "Wrap errors.", Paths: []string{}, Source: "account", Repositories: []string{"alpha/two"}},
		{
			Kind: RuleWritten, ID: "wrap-errors", Text: "Wrap errors here too.", Paths: []string{"**/*.go"}, Source: RuleFromEntry,
			Repositories: []string{"alpha/one"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collectRules =\n%+v\nwant\n%+v", got, want)
	}
}
