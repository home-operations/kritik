package repoconfig

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMerge(t *testing.T) {
	t.Parallel()
	op := Operator{
		Enabled: true, Ignore: []string{"vendor/**"}, Instructions: []string{"docs/rules.md"}, RequireSuggestedFix: true,
		Templates: Templates{Summary: "docs/summary.j2"},
	}
	tests := []struct {
		name    string
		doc     string
		want    Operator
		filter  bool
		skip    []string
		wantErr string
	}{
		{name: "no file", want: op},
		{
			name: "the file narrows and replaces presentation",
			doc: "enabled: false\nfilter: '!pr.draft'\nignore: [gen/**, vendor/**]\nskip:\n  onlyPaths: [docs/**]\n" +
				"review:\n  instructions: [.kritik/rules.md]\n  requireSuggestedFix: false\n  templates:\n    inline: .kritik/inline.j2\n",
			want: Operator{Ignore: []string{"vendor/**", "gen/**"}, Instructions: []string{".kritik/rules.md"},
				Templates: Templates{Summary: "docs/summary.j2", Inline: ".kritik/inline.j2"}},
			filter: true, skip: []string{"docs/**"},
		},
		{name: "enabled true cannot widen", doc: "enabled: true\n", want: op},
		{name: "a file that does not parse leaves the operator's settings", doc: "unknown: 1\n", want: op, wantErr: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := Files{}
			if tt.doc != "" {
				files[FileName] = tt.doc
			}
			m, err := Merge(files, op)
			if (err != nil) != (tt.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if m.Enabled != tt.want.Enabled || !slices.Equal(m.Ignore, tt.want.Ignore) || !slices.Equal(m.Instructions, tt.want.Instructions) ||
				m.RequireSuggestedFix != tt.want.RequireSuggestedFix || m.Templates != tt.want.Templates ||
				(m.Filter != nil) != tt.filter || !slices.Equal(m.Skip.OnlyPaths, tt.skip) {
				t.Fatalf("Merge = %+v", m)
			}
		})
	}
	if _, err := Merge(Files{FileName: "ignore: [x/**]\n"}, op); err != nil || len(op.Ignore) != 1 {
		t.Fatalf("Merge must not change the operator's ignore list: %v %v", op.Ignore, err)
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
			m, err := Merge(Files{FileName: tt.doc}, Operator{Enabled: true})
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
		CreatedAt: at, Labels: []byte(`[{"name":"deps","color":"ededed"}]`)}
	vars, err := pr.Vars()
	if err != nil {
		t.Fatal(err)
	}
	if vars["number"] != 7 || vars["open"] != false || vars["merged"] != true || vars["body"] != "Adds b." ||
		vars["createdAt"] != at || vars["headSha"] != "abc" || len(vars["labels"].([]any)) != 1 || len(vars) != 15 {
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
