package agent

import "testing"

func TestGHSource(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"api", "repos/a/b/compare/v1...v2"}, "https://api.github.com/repos/a/b/compare/v1...v2"},
		{[]string{"api", "-H", "Accept: application/vnd.github+json", "/repos/a/b/releases/tags/v2", "--jq", ".body"},
			"https://api.github.com/repos/a/b/releases/tags/v2"},
		{[]string{"api", "graphql", "-f", "query=x"}, ""},
		{[]string{"api", "repos/{owner}/{repo}/pulls"}, ""},
		{[]string{"release", "view", "v2", "-R", "a/b"}, "https://github.com/a/b/releases/tag/v2"},
		{[]string{"release", "view", "--repo=a/b", "--json", "body"}, "https://github.com/a/b/releases/latest"},
		{[]string{"release", "list", "--repo", "a/b", "-L", "5"}, "https://github.com/a/b/releases"},
		{[]string{"pr", "view", "12", "-R", "a/b", "--comments"}, "https://github.com/a/b/pull/12"},
		{[]string{"pr", "diff", "12", "-R", "a/b"}, "https://github.com/a/b/pull/12"},
		{[]string{"issue", "view", "7", "-R", "a/b"}, "https://github.com/a/b/issues/7"},
		{[]string{"repo", "view", "a/b"}, "https://github.com/a/b"},
		// Nothing to name: no repository, or a call that reads no page.
		{[]string{"release", "view", "v2"}, ""},
		{[]string{"pr", "view", "12", "-R", "a"}, ""},
		{[]string{"search", "repos", "kritik"}, ""},
		{[]string{"--version"}, ""},
	} {
		if got := ghSource(tt.args); got != tt.want {
			t.Errorf("ghSource(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}
