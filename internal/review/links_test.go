package review

import (
	"slices"
	"testing"
)

func TestSourceLinks(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"https://api.github.com/repos/stakater/Reloader/compare/chart-v2.2.17...chart-v2.2.18",
			"https://redirect.github.com/stakater/Reloader/compare/chart-v2.2.17...chart-v2.2.18"},
		{"http://api.github.com/repos/a/b/releases/tags/v2", "https://redirect.github.com/a/b/releases/tag/v2"},
		{"https://api.github.com/repos/a/b/releases?per_page=10", "https://redirect.github.com/a/b/releases"},
		{"https://api.github.com/repos/a/b/releases/latest", "https://redirect.github.com/a/b/releases/latest"},
		{"https://api.github.com/repos/a/b/pulls/1234/files", "https://redirect.github.com/a/b/pull/1234"},
		{"https://api.github.com/repos/a/b/issues/7/comments", "https://redirect.github.com/a/b/issues/7"},
		{"https://api.github.com/repos/a/b/commits/abc123", "https://redirect.github.com/a/b/commit/abc123"},
		{"https://api.github.com/repos/a/b/contents/charts/x/Chart.yaml?ref=v2", "https://redirect.github.com/a/b/blob/v2/charts/x/Chart.yaml"},
		{"https://api.github.com/repos/a/b/contents/README.md", "https://redirect.github.com/a/b/blob/HEAD/README.md"},
		{"https://api.github.com/repos/a/b/tags?per_page=5", "https://redirect.github.com/a/b/tags"},
		{"https://api.github.com/repos/a/b/git/refs/tags", "https://redirect.github.com/a/b"},
		{"https://github.com/a/b/pull/12#issuecomment-1", "https://redirect.github.com/a/b/pull/12#issuecomment-1"},
		{"http://GitHub.com/a/b/releases/tag/v2", "https://redirect.github.com/a/b/releases/tag/v2"},
		// No page to link, or not GitHub: the URL as it was fetched.
		{"https://api.github.com/search/issues?q=x", "https://api.github.com/search/issues?q=x"},
		{"https://raw.githubusercontent.com/a/b/v2/Chart.yaml", "https://raw.githubusercontent.com/a/b/v2/Chart.yaml"},
		{"https://releases.example.com/b/v2", "https://releases.example.com/b/v2"},
		{"not a url", "not a url"},
	} {
		if got := SourceLinks([]string{tt.in}); !slices.Equal(got, []string{tt.want}) {
			t.Errorf("SourceLinks(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSourceLinksDropsRepeats(t *testing.T) {
	got := SourceLinks([]string{
		"https://api.github.com/repos/a/b/compare/v1...v2", "https://releases.example.com/x",
		"https://github.com/a/b/compare/v1...v2",
	})
	want := []string{"https://redirect.github.com/a/b/compare/v1...v2", "https://releases.example.com/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("SourceLinks = %q, want %q", got, want)
	}
}

func TestRedirectReferences(t *testing.T) {
	const repo = "me/home"
	for _, tt := range []struct {
		name, in, want string
	}{
		{"shorthand", "fixed in home-operations/kritika#362.",
			"fixed in [home-operations/kritika#362](https://redirect.github.com/home-operations/kritika/issues/362)."},
		{"shorthand at the start", "a/b#1 says so", "[a/b#1](https://redirect.github.com/a/b/issues/1) says so"},
		{"shorthand with a dotted repository", "see a/b.js#12\n", "see [a/b.js#12](https://redirect.github.com/a/b.js/issues/12)\n"},
		{"url", "see https://github.com/a/b/pull/12/files for the diff",
			"see https://redirect.github.com/a/b/pull/12/files for the diff"},
		{"url in a link", "[the fix](https://github.com/a/b/issues/7#issuecomment-1)",
			"[the fix](https://redirect.github.com/a/b/issues/7#issuecomment-1)"},
		{"url in an autolink", "<https://GitHub.com/a/b/compare/v1...v2>", "<https://redirect.github.com/a/b/compare/v1...v2>"},
		{"own repository", "as me/home#3 and https://github.com/me/home/pull/4 and ME/Home#5 say",
			"as me/home#3 and https://github.com/me/home/pull/4 and ME/Home#5 say"},
		{"same-repository shorthand", "fixes #12 and GH-13", "fixes #12 and GH-13"},
		{"not a reference", "a/b#x, a/b/c#1, v1.2/b#3, @a/b#4, http://x.com/a/b#5",
			"a/b#x, a/b/c#1, v1.2/b#3, @a/b#4, http://x.com/a/b#5"},
		{"code span", "see `a/b#1` and ``a/b#2 `x` `` but a/b#3",
			"see `a/b#1` and ``a/b#2 `x` `` but [a/b#3](https://redirect.github.com/a/b/issues/3)"},
		{"unclosed backtick is text", "a ` b a/b#1", "a ` b [a/b#1](https://redirect.github.com/a/b/issues/1)"},
		{"fenced block", "a/b#1\n```go\n// a/b#2 https://github.com/a/b/pull/3\n```\na/b#4\n~~~\na/b#5\n",
			"[a/b#1](https://redirect.github.com/a/b/issues/1)\n```go\n// a/b#2 https://github.com/a/b/pull/3\n```\n" +
				"[a/b#4](https://redirect.github.com/a/b/issues/4)\n~~~\na/b#5\n"},
		{"indented fence", "   ```\n a/b#1\n  ```\na/b#2", "   ```\n a/b#1\n  ```\n[a/b#2](https://redirect.github.com/a/b/issues/2)"},
		{"longer closing fence needed", "````\n```\na/b#1\n````\na/b#2", "````\n```\na/b#1\n````\n[a/b#2](https://redirect.github.com/a/b/issues/2)"},
		{"backticks mid-line are not a fence", "x ``` a/b#1", "x ``` [a/b#1](https://redirect.github.com/a/b/issues/1)"},
		{"already linked", "[a/b#1](https://redirect.github.com/a/b/issues/1)", "[a/b#1](https://redirect.github.com/a/b/issues/1)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := RedirectReferences(tt.in, repo)
			if got != tt.want {
				t.Errorf("RedirectReferences(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
			if again := RedirectReferences(got, repo); again != got {
				t.Errorf("not idempotent:\n got %q\nwant %q", again, got)
			}
		})
	}
}
