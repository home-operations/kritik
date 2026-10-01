package repoconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/webhook"
)

func adminSettings() configfile.Settings {
	return configfile.Settings{
		Enabled: true, Ignore: []string{"vendor/**"}, Settle: 2 * time.Minute,
		Models: configfile.Models{Review: "p/big"},
		Agent:  configfile.AgentSettings{MaxSteps: 30, MaxToolOutputBytes: 1000, MaxTokens: 5000, Timeout: 10 * time.Minute, Commands: []string{"rg"}},
		Review: configfile.Review{
			RequireSuggestedFix: true,
			Templates:           configfile.ReviewTemplates{Summary: "docs/summary.tmpl"}, InlineComments: true,
			Rules: []configfile.Rule{{ID: "wrap-errors", Rule: "Wrap errors."}, {ID: "house-style", File: "docs/rules.md"}}, AgentFiles: true,
		},
		Providers: []string{"own", "p"},
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		doc     string
		want    func(*configfile.Settings)
		filter  bool
		dropped []string
		wantErr string
	}{
		{name: "no file"},
		{
			name: "the file narrows, appends file rules and replaces presentation",
			doc: "enabled: false\nfilterExpr: '!pr.draft'\nignore: [gen/**, vendor/**]\n" +
				"rules: [{ id: repo-style, file: .kritika/rules.md }, { id: sql, file: .kritika/sql.md, paths: ['**/*.sql'] }]\n" +
				"comments:\n  inlineTemplate: .kritika/inline.tmpl\n",
			want: func(s *configfile.Settings) {
				s.Enabled, s.Ignore = false, []string{"vendor/**", "gen/**"}
				s.Review.Rules = append(s.Review.Rules, configfile.Rule{ID: "repo-style", File: ".kritika/rules.md"},
					configfile.Rule{ID: "sql", File: ".kritika/sql.md", Paths: []string{"**/*.sql"}})
				s.Review.Templates.Inline = ".kritika/inline.tmpl"
			},
			filter: true,
		},
		{
			name: "an admin's file rule stays as the admin wrote it", doc: "rules: [{ id: house-style, file: docs/rules.md, paths: ['**/*.sql'] }]\n",
			dropped: []string{".kritika.yaml: rules house-style was dropped: an admin's rule has that id"},
		},
		{
			name: "context files follow the admin's", doc: "context: [{ path: db/schema.sql, description: the schema, paths: ['**/*.sql'] }]\n",
			want: func(s *configfile.Settings) {
				s.Review.Context = append(s.Review.Context, configfile.ContextFile{Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}})
			},
		},
		{name: "a context file without a description", doc: "context: [{ path: db/schema.sql }]\n", wantErr: "description is required"},
		{
			name: "rules follow the admin's, and one with an admin's id is dropped",
			doc:  "rules:\n  - { id: wrap-errors, rule: Anything goes. }\n  - { id: no-tokens, rule: Never log a token., paths: ['**/*.go'] }\n",
			want: func(s *configfile.Settings) {
				s.Review.Rules = append(s.Review.Rules, configfile.Rule{ID: "no-tokens", Rule: "Never log a token.", Paths: []string{"**/*.go"}})
			},
			dropped: []string{".kritika.yaml: rules wrap-errors was dropped: an admin's rule has that id"},
		},
		{name: "a rule without an id", doc: "rules: [{ rule: Never log a token. }]\n", wantErr: `rules[0].id "" must be`},
		{
			name: "a rule may say when it applies",
			doc:  "rules: [{ id: renovate, rule: Say what breaks., whenExpr: 'pr.headRef.startsWith(\"renovate/\")' }]\n",
			want: func(s *configfile.Settings) {
				s.Review.Rules = append(s.Review.Rules, configfile.Rule{ID: "renovate", Rule: "Say what breaks.", WhenExpr: `pr.headRef.startsWith("renovate/")`})
			},
		},
		{name: "a rule whose whenExpr fails the smoke test", doc: "rules: [{ id: a, rule: x, whenExpr: 'pr.labels[5].name == \"x\"' }]\n", wantErr: "rules[0].whenExpr: smoke test"},
		{name: "enabled true cannot widen", doc: "enabled: true\n"},
		{
			name: "presentation replaces the admin's", doc: "comments: { inline: false, summaryTemplate: .kritika/summary.tmpl }\nagentFiles: false\n",
			want: func(s *configfile.Settings) {
				s.Review.InlineComments, s.Review.AgentFiles, s.Review.Templates.Summary = false, false, ".kritika/summary.tmpl"
			},
		},
		{
			name: "feedback replaces the admin's", doc: "feedback: minimal\n",
			want: func(s *configfile.Settings) { s.Review.Feedback = configfile.FeedbackMinimal },
		},
		{
			name: "an unknown feedback level is dropped", doc: "feedback: exhaustive\n",
			dropped: []string{`.kritika.yaml: feedback "exhaustive" was dropped; allowed: detailed, standard or minimal`},
		},
		{
			name: "approve replaces the admin's", doc: "approve: true\n",
			want: func(s *configfile.Settings) { s.Review.Approve = true },
		},
		{
			name: "requireSuggestedFix may only turn on", doc: "requireSuggestedFix: false\n",
			dropped: []string{".kritika.yaml: requireSuggestedFix false was dropped; allowed: true, since an admin requires a suggested fix"},
		},
		{
			name: "a model of a provider the account may use",
			doc:  "models: { review: own/small, fallback: p/big }\n",
			want: func(s *configfile.Settings) {
				s.Models = configfile.Models{Review: "own/small", Fallback: "p/big"}
			},
		},
		{
			name: "a model of another provider, or no model, is dropped", doc: "models: { review: q/big, fallback: p }\n",
			dropped: []string{
				`.kritika.yaml: models.review "q/big" was dropped; allowed: a model of own, p`,
				`.kritika.yaml: models.fallback "p" was dropped; allowed: a model of own, p`,
			},
		},
		{name: "a mode is no longer a key", doc: "mode: agentic\n", wantErr: "field mode not found"},
		{name: "agent limits are the admin's alone", doc: "agent: { maxSteps: 5 }\n", wantErr: "field agent not found"},
		{name: "settle is the admin's alone", doc: "settle: 1m\n", wantErr: "field settle not found"},
		{name: "a secret reference does not decode", doc: "models: { review: { env: KEY } }\n", wantErr: "cannot unmarshal"},
		{name: "a file that does not parse leaves the admin's settings", doc: "unknown: 1\n", wantErr: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var doc []byte
			if tt.doc != "" {
				doc = []byte(tt.doc)
			}
			op, want := adminSettings(), adminSettings()
			if tt.want != nil {
				tt.want(&want)
			}
			m, err := Merge(doc, op)
			if (err != nil) != (tt.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(m.Settings, want) {
				t.Fatalf("settings = %+v\nwant       %+v", m.Settings, want)
			}
			if (m.InRepoFilter != nil) != tt.filter || !slices.Equal(m.Dropped, tt.dropped) {
				t.Fatalf("filter=%v dropped=%q", m.InRepoFilter != nil, m.Dropped)
			}
			if !reflect.DeepEqual(op, adminSettings()) {
				t.Fatal("Merge changed the admin's settings")
			}
		})
	}
}

func TestMergedCheck(t *testing.T) {
	t.Parallel()
	pr := PullRequest{Number: 3, Title: "t", Body: "please [skip-review]", State: "open", Labels: []byte(`[{"name":"deps"}]`)}
	vars, err := pr.Vars()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		doc     string
		changed []string
		want    SkipReason
		wantErr bool
	}{
		{"nothing to skip", "", []string{"main.go"}, "", false},
		{"disabled", "enabled: false\n", []string{"main.go"}, SkipDisabled, false},
		{"filtered", "filterExpr: '!pr.body.contains(\"[skip-review]\")'\n", []string{"main.go"}, SkipFiltered, false},
		{"filter allows", "filterExpr: 'pr.number == 3 && pr.open && pr.labels[0].name == \"deps\"'\n", []string{"main.go"}, "", false},
		{"filter that fails to evaluate skips", "filterExpr: 'pr.number == 1 || pr.labels[9].name == \"x\"'\n", []string{"main.go"}, SkipFiltered, true},
		{"only ignored paths", "ignore: [docs/**]\n", []string{"docs/a.md"}, SkipOnlyPaths, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, err := Merge([]byte(tt.doc), configfile.Settings{Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.Check(vars, tt.changed)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("Check = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	for r, want := range map[SkipReason]string{
		SkipDisabled: "disabled in .kritika.yaml", SkipFiltered: "filtered by .kritika.yaml", SkipOnlyPaths: "only ignored paths changed",
	} {
		if !r.Valid() || r.Description() != want {
			t.Fatalf("%q.Description() = %q, want %q", r, r.Description(), want)
		}
	}
	if SkipReason("other").Valid() {
		t.Fatal("an unknown reason must not be valid")
	}
}

func TestPullRequestVars(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	pr := PullRequest{Number: 7, Title: "Add b", Author: "octocat", State: "closed", Merged: true, Draft: true, Fork: true,
		HeadRef: "f", HeadSHA: "abc", BaseRef: "main", URL: "https://forge.example.com/acme/widgets/pulls/7", Body: "Adds b.",
		CreatedAt: at, Labels: []byte(`[{"name":"deps","color":"ededed"}]`), Event: "manual"}
	vars, err := pr.Vars()
	if err != nil {
		t.Fatal(err)
	}
	if vars["number"] != 7 || vars["open"] != false || vars["merged"] != true || vars["body"] != "Adds b." ||
		vars["createdAt"] != at || vars["headSha"] != "abc" || len(vars["labels"].([]any)) != 1 || vars["event"] != "manual" || len(vars) != 16 {
		t.Fatalf("vars = %v", vars)
	}
	// A pull request that crossed a JSON job document keeps its types.
	var back PullRequest
	if err := jsonRoundTrip(pr, &back); err != nil {
		t.Fatal(err)
	}
	again, err := back.Vars()
	if err != nil || again["number"] != 7 || again["createdAt"] != at {
		t.Fatalf("round trip vars = %v, %v", again, err)
	}
	if empty, err := (PullRequest{}).Vars(); err != nil || len(empty["labels"].([]any)) != 0 {
		t.Fatalf("empty labels = %v, %v", empty["labels"], err)
	}
}

func jsonRoundTrip(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// TestVarsMatchFilterVars: a filter sees the same pr variable when ingest
// judges a webhook's pull request as when the worker judges its stored row.
func TestVarsMatchFilterVars(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hook := webhook.PullRequest{
		Number: 7, Title: "t", Author: "a", State: "open", Merged: true, Draft: true, Fork: true,
		HeadRef: "f", HeadSHA: "abc", BaseRef: "main", URL: "https://x", Body: "b", CreatedAt: at,
		Labels: []webhook.Label{{Name: "bug", Color: "f00"}},
	}
	labels, err := json.Marshal(hook.LabelVars())
	if err != nil {
		t.Fatal(err)
	}
	stored := PullRequest{
		Number: 7, Title: "t", Author: "a", State: "open", Merged: true, Draft: true, Fork: true,
		HeadRef: "f", HeadSHA: "abc", BaseRef: "main", URL: "https://x", Body: "b", CreatedAt: at,
		Labels: labels, Event: "opened",
	}
	got, err := stored.Vars()
	if err != nil {
		t.Fatal(err)
	}
	if want := hook.FilterVars("opened"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Vars = %v\nFilterVars = %v", got, want)
	}
}
