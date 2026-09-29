package repoconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/webhook"
)

func adminSettings() configfile.Settings {
	return configfile.Settings{
		Enabled: true, Ignore: []string{"vendor/**"}, Mode: configfile.ReviewSingle, Settle: 2 * time.Minute,
		Models: configfile.Models{Review: "p/big"},
		Agent:  configfile.AgentSettings{MaxSteps: 30, MaxToolOutputBytes: 1000, MaxTokens: 5000, Timeout: 10 * time.Minute, Commands: []string{"rg"}},
		Review: configfile.Review{
			Instructions: []string{"docs/rules.md"}, RequireSuggestedFix: true,
			Templates: configfile.ReviewTemplates{Summary: "docs/summary.tmpl"}, InlineComments: true,
		},
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		doc     string
		allow   configfile.Allow
		want    func(*configfile.Settings)
		filter  bool
		skip    []string
		scoped  map[string][]string
		dropped []string
		wantErr string
	}{
		{name: "no file"},
		{
			name: "the file narrows, appends instructions and replaces presentation",
			doc: "enabled: false\nfilter: '!pr.draft'\nignore: [gen/**, vendor/**]\nskip:\n  onlyPaths: [docs/**]\n" +
				"review:\n  instructions: [.kritik/rules.md, docs/rules.md, { path: .kritik/sql.md, paths: ['**/*.sql'] }]\n" +
				"  templates:\n    inline: .kritik/inline.tmpl\n",
			want: func(s *configfile.Settings) {
				s.Enabled, s.Ignore = false, []string{"vendor/**", "gen/**"}
				s.Review.Instructions = []string{"docs/rules.md", ".kritik/rules.md", ".kritik/sql.md"}
				s.Review.Templates.Inline = ".kritik/inline.tmpl"
			},
			filter: true, skip: []string{"docs/**"}, scoped: map[string][]string{".kritik/sql.md": {"**/*.sql"}},
		},
		{
			name: "an admin's instruction stays unscoped", doc: "review:\n  instructions: [{ path: docs/rules.md, paths: ['**/*.sql'] }]\n",
		},
		{
			name: "context files follow the admin's", doc: "review:\n  context: [{ path: db/schema.sql, description: the schema, paths: ['**/*.sql'] }]\n",
			want: func(s *configfile.Settings) {
				s.Review.Context = append(s.Review.Context, configfile.ContextFile{Path: "db/schema.sql", Description: "the schema", Paths: []string{"**/*.sql"}})
			},
		},
		{name: "a context file without a description", doc: "review:\n  context: [{ path: db/schema.sql }]\n", wantErr: "description is required"},
		{name: "enabled true cannot widen", doc: "enabled: true\n"},
		{
			name: "presentation replaces the admin's", doc: "review: { minSeverity: important, inlineComments: false }\n",
			want: func(s *configfile.Settings) {
				s.Review.MinSeverity, s.Review.InlineComments = configfile.SeverityImportant, false
			},
		},
		{
			name: "an unknown severity floor is dropped", doc: "review: { minSeverity: blocking }\n",
			dropped: []string{`.kritik.yaml: review.minSeverity "blocking" was dropped; allowed: nit, important`},
		},
		{
			name: "requireSuggestedFix may only turn on", doc: "review:\n  requireSuggestedFix: false\n",
			dropped: []string{".kritik.yaml: review.requireSuggestedFix false was dropped; allowed: true, since an admin requires a suggested fix"},
		},
		{
			name: "with no bounds set, the admin's own values or lower",
			doc:  "mode: single\nmodels: { review: p/big }\nagent: { maxSteps: 20, maxTokens: 5000, commands: [] }\nsettle: 30s\n",
			want: func(s *configfile.Settings) {
				s.Agent.MaxSteps, s.Agent.Commands, s.Settle = 20, []string{}, 30*time.Second
			},
		},
		{
			name: "with no bounds set, anything else is dropped",
			doc:  "mode: agentic\nmodels: { review: p/small, fallback: p/big }\nagent: { maxSteps: 31, timeout: 0s, commands: [rg, curl] }\nsettle: 3m\n",
			dropped: []string{
				`.kritik.yaml: mode "agentic" was dropped; allowed: single`,
				`.kritik.yaml: models.review "p/small" was dropped; allowed: p/big`,
				`.kritik.yaml: models.fallback "p/big" was dropped; allowed: none`,
				`.kritik.yaml: agent.commands "curl" was dropped; allowed: rg`,
				".kritik.yaml: agent.maxSteps 31 was dropped; allowed: above 0, at most 30",
				".kritik.yaml: agent.timeout 0s was dropped; allowed: above 0, at most 10m0s",
				".kritik.yaml: settle 3m0s was dropped; allowed: 0s to 2m0s",
			},
		},
		{
			name: "the bounds open choices past the admin's own",
			doc:  "mode: agentic\nmodels: { review: p/small, fallback: p/big }\nagent: { maxSteps: 60, timeout: 20m, commands: [fd, curl] }\nsettle: 30m\n",
			allow: configfile.Allow{
				Modes: []configfile.ReviewMode{configfile.ReviewSingle, configfile.ReviewAgentic}, Models: []configfile.ModelRef{"p/big", "p/small"},
				Commands: []string{"rg", "fd", "curl"}, Agent: configfile.AllowAgent{MaxSteps: new(60), Timeout: new(20 * time.Minute)},
				Settle: new(30 * time.Minute),
			},
			want: func(s *configfile.Settings) {
				s.Mode, s.Models = configfile.ReviewAgentic, configfile.Models{Review: "p/small", Fallback: "p/big"}
				s.Agent.MaxSteps, s.Agent.Timeout, s.Agent.Commands = 60, 20*time.Minute, []string{"fd", "curl"}
				s.Settle = 30 * time.Minute
			},
		},
		{
			name: "a value past its bound is dropped, not clamped", doc: "agent: { maxTokens: 9000 }\nsettle: 31m\n",
			allow: configfile.Allow{Agent: configfile.AllowAgent{MaxTokens: new(int64(8000))}, Settle: new(30 * time.Minute)},
			dropped: []string{
				".kritik.yaml: agent.maxTokens 9000 was dropped; allowed: above 0, at most 8000",
				".kritik.yaml: settle 31m0s was dropped; allowed: 0s to 30m0s",
			},
		},
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
			op := adminSettings()
			op.Allow = tt.allow
			want := adminSettings()
			want.Allow = tt.allow
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
			if (m.InRepoFilter != nil) != tt.filter || !slices.Equal(m.Skip.OnlyPaths, tt.skip) || !slices.Equal(m.Dropped, tt.dropped) ||
				!reflect.DeepEqual(m.Scoped, tt.scoped) {
				t.Fatalf("filter=%v skip=%v scoped=%v dropped=%q", m.InRepoFilter != nil, m.Skip.OnlyPaths, m.Scoped, m.Dropped)
			}
			if !reflect.DeepEqual(op, func() configfile.Settings { o := adminSettings(); o.Allow = tt.allow; return o }()) {
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
		{"filtered", "filter: '!pr.body.contains(\"[skip-review]\")'\n", []string{"main.go"}, SkipFiltered, false},
		{"filter allows", "filter: 'pr.number == 3 && pr.open && pr.labels[0].name == \"deps\"'\n", []string{"main.go"}, "", false},
		{"filter that fails to evaluate skips", "filter: 'pr.number == 1 || pr.labels[9].name == \"x\"'\n", []string{"main.go"}, SkipFiltered, true},
		{"only skipped paths", "skip:\n  onlyPaths: [docs/**]\n", []string{"docs/a.md"}, SkipOnlyPaths, false},
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
		SkipDisabled: "disabled in .kritik.yaml", SkipFiltered: "filtered by .kritik.yaml", SkipOnlyPaths: "only skipped paths changed",
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
