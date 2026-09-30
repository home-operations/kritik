package review

import (
	"cmp"
	"net/url"
	"slices"
	"strings"
)

// redirectHost serves github.com's pages under another name. A github.com
// link to an issue or pull request makes GitHub add a reference to it on
// the linked page, so a review citing an upstream would add one to every
// project it read; Renovate and Dependabot link through redirect.github.com
// for the same reason.
const redirectHost = "redirect.github.com"

// SourceLinks is urls as links for a comment, in order and without
// repeats: a github.com page, or the page of an api.github.com resource of
// a repository, on redirect.github.com, and any other URL as it is.
func SourceLinks(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, s := range urls {
		if l := sourceLink(s); !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func sourceLink(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return s
	}
	link := url.URL{Scheme: "https", Host: redirectHost}
	switch strings.ToLower(u.Hostname()) {
	case "github.com":
		link.Path, link.RawQuery, link.Fragment = u.Path, u.RawQuery, u.Fragment
	case "api.github.com":
		page, ok := apiPage(u)
		if !ok {
			return s
		}
		link.Path = page
	default:
		return s
	}
	return link.String()
}

// apiPage is the github.com path of the page showing what u, a REST API
// URL under /repos/{owner}/{repo}, returns: the repository itself when
// the resource has no page of its own.
func apiPage(u *url.URL) (string, bool) {
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) < 3 || seg[0] != "repos" {
		return "", false
	}
	repo, rest := "/"+seg[1]+"/"+seg[2], seg[3:]
	if len(rest) == 0 {
		return repo, true
	}
	switch {
	case rest[0] == "compare" && len(rest) > 1:
		return repo + "/compare/" + strings.Join(rest[1:], "/"), true
	case rest[0] == "releases":
		switch {
		case len(rest) > 2 && rest[1] == "tags":
			return repo + "/releases/tag/" + strings.Join(rest[2:], "/"), true
		case len(rest) == 2 && rest[1] == "latest":
			return repo + "/releases/latest", true
		}
		return repo + "/releases", true
	case rest[0] == "pulls" && len(rest) > 1:
		return repo + "/pull/" + rest[1], true
	case rest[0] == "issues" && len(rest) > 1:
		return repo + "/issues/" + rest[1], true
	case rest[0] == "commits" && len(rest) > 1:
		return repo + "/commit/" + rest[1], true
	case rest[0] == "contents" && len(rest) > 1:
		return repo + "/blob/" + cmp.Or(u.Query().Get("ref"), "HEAD") + "/" + strings.Join(rest[1:], "/"), true
	case rest[0] == "tags":
		return repo + "/tags", true
	}
	return repo, true
}
