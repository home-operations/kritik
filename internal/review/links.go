package review

import (
	"cmp"
	"net/url"
	"regexp"
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

var (
	// githubURL is a github.com URL in prose: it ends at whitespace or at
	// a character that closes a markdown link, autolink or quote.
	githubURL = regexp.MustCompile(`(?i)https?://github\.com/[^\s<>"'()\[\]]*`)
	// shortRef is GitHub's owner/repo#N shorthand, between characters that
	// are not part of a name or a URL path.
	shortRef = regexp.MustCompile(`(?:^|[^\w/.@-])([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+)#(\d+)(?:$|[^\w])`)
)

// RedirectReferences rewrites the GitHub references in text, markdown the
// model wrote, so the forge does not add a cross-reference to each page
// they name: a github.com URL moves to redirect.github.com as SourceLinks
// does, and owner/repo#N becomes a link to that issue or pull request
// there (GitHub redirects /issues/N to the pull request when N is one).
// References to repository, the "owner/repo" under review, are left
// alone: a cross-reference there is the author's own. So are references
// in code spans and fenced blocks, which the forge does not link, and the
// text of a link already made, which keeps a second pass from nesting
// one.
func RedirectReferences(text, repository string) string {
	return mapProse(text, func(s string) string {
		s = githubURL.ReplaceAllStringFunc(s, func(m string) string {
			u, err := url.Parse(m)
			if err != nil || ownRepository(u.Path, repository) {
				return m
			}
			return sourceLink(m)
		})
		var b strings.Builder
		last := 0
		for _, i := range shortRef.FindAllStringSubmatchIndex(s, -1) {
			start, end := i[2], i[5]
			ref := s[start:end]
			if ownRepository("/"+s[i[2]:i[3]], repository) || strings.HasPrefix(s[end:], "](") && start > 0 && s[start-1] == '[' {
				continue
			}
			link := sourceLink("https://github.com/" + s[i[2]:i[3]] + "/issues/" + s[i[4]:i[5]])
			b.WriteString(s[last:start])
			b.WriteString("[" + ref + "](" + link + ")")
			last = end
		}
		b.WriteString(s[last:])
		return b.String()
	})
}

// ownRepository is whether path, a github.com path, is repository's page
// or one under it.
func ownRepository(path, repository string) bool {
	if repository == "" {
		return false
	}
	path, prefix := strings.ToLower(path), "/"+strings.ToLower(repository)
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// mapProse applies fn to the parts of text outside code spans and fenced
// code blocks, as CommonMark reads them: a fence of three or more
// backticks or tildes opening a line runs to a line that is a fence of
// the same character at least as long, or to the end; a run of backticks
// opens a span closed by the next run of the same length, and one with no
// closer is text.
func mapProse(text string, fn func(string) string) string {
	var b strings.Builder
	prose := 0 // start of the prose not yet mapped
	flush := func(end int) {
		b.WriteString(fn(text[prose:end]))
		prose = end
	}
	atLineStart := true
	for i := 0; i < len(text); {
		c := text[i]
		if atLineStart {
			if n, ok := fenceAt(text, i); ok {
				end := fenceEnd(text, i+n, c, n)
				flush(i)
				b.WriteString(text[i:end])
				prose, i = end, end
				continue
			}
		}
		if c == '`' {
			n := run(text, i, '`')
			if end := spanEnd(text, i+n, n); end > 0 {
				flush(i)
				b.WriteString(text[i:end])
				prose, i = end, end
				atLineStart = false
				continue
			}
			i += n
			atLineStart = false
			continue
		}
		atLineStart = c == '\n' || (atLineStart && c == ' ' && i-lineStart(text, i) < 3)
		i++
	}
	flush(len(text))
	return b.String()
}

// fenceAt is the length of the fence opening at i, past the indentation
// a line start allows, when one does.
func fenceAt(text string, i int) (int, bool) {
	if i >= len(text) || (text[i] != '`' && text[i] != '~') {
		return 0, false
	}
	n := run(text, i, text[i])
	return n, n >= 3
}

// fenceEnd is the index just past the closing fence of the block opened
// at i by n characters c, or len(text) when it never closes.
func fenceEnd(text string, i int, c byte, n int) int {
	for {
		nl := strings.IndexByte(text[i:], '\n')
		if nl < 0 {
			return len(text)
		}
		i += nl + 1
		line := text[i:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 {
			continue
		}
		if m := run(trimmed, 0, c); m >= n && strings.TrimSpace(trimmed[m:]) == "" {
			return min(len(text), i+len(line)+1)
		}
	}
}

// spanEnd is the index just past the run of exactly n backticks that
// closes the span opened before i, or 0 when there is none.
func spanEnd(text string, i, n int) int {
	for i < len(text) {
		j := strings.IndexByte(text[i:], '`')
		if j < 0 {
			return 0
		}
		m := run(text, i+j, '`')
		if m == n {
			return i + j + m
		}
		i += j + m
	}
	return 0
}

// run is the length of the run of c starting at i.
func run(text string, i int, c byte) int {
	n := 0
	for i+n < len(text) && text[i+n] == c {
		n++
	}
	return n
}

// lineStart is the index of the first character of the line holding i.
func lineStart(text string, i int) int {
	return strings.LastIndexByte(text[:i], '\n') + 1
}
