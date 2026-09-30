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
