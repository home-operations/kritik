//go:build bench

// Command mine builds corpus cases from a repository's history: for every
// fix commit on the main branch it blames the lines the fix changed back
// to the commit that introduced them; when that commit is itself a squash
// merged pull request, that PR becomes a case whose expected finding is
// the introduced lines, with the fix as the description.
//
//	go run -tags bench ./bench/cmd/mine -repo ../flate -name home-operations/flate -out bench/cases/flate.yaml
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/bench"
)

var (
	prNumber = regexp.MustCompile(`\(#(\d+)\)$`)
	// Dependency and tooling bumps fix nothing a reviewer could have seen.
	skipSubject = regexp.MustCompile(
		`(?i)update (module|image|tool|dependency|kubernetes|crate|helm|github|action)|^(chore|docs|ci|build|test|style)(\(|:)|\(deps\)|^release`,
	)
	hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)
	// Generated, vendored or prose paths: a fix touching them is not a
	// defect a reviewer could have caught in the introducing diff.
	skipPath = regexp.MustCompile(
		`(?i)(^|/)(go\.sum|go\.mod|package-lock\.json|[^/]+\.lock|changelog\.md|readme\.md)$` +
			`|(^|/)(values\.schema\.json|\.release-please-manifest\.json)$` +
			`|\.(md|json|css|svg|snap|txt)$|^\.github/|^\.mise/|(^|/)__snapshot__/`,
	)
)

func main() {
	repo := flag.String("repo", "", "path of a local clone")
	name := flag.String("name", "", "owner/name on GitHub")
	ref := flag.String("ref", "origin/main", "branch to walk")
	depth := flag.Int("depth", 400, "how many commits back to look")
	out := flag.String("out", "", "corpus file to write (stdout when empty)")
	flag.Parse()
	if *repo == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "mine: -repo and -name are required")
		os.Exit(2)
	}
	cases, err := mine(*repo, *name, *ref, *depth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mine:", err)
		os.Exit(1)
	}
	raw, err := yaml.Marshal(bench.File{Cases: cases})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mine:", err)
		os.Exit(1)
	}
	header := "# Mined by `go run -tags bench ./bench/cmd/mine`: each case is a squash merged\n" +
		"# pull request whose lines a later fix commit changed. Curate by hand: demote\n" +
		"# `must` where the defect was not visible in the diff, and add notes.\n"
	if *out == "" {
		fmt.Print(header + string(raw))
		return
	}
	if err := os.WriteFile(*out, []byte(header+string(raw)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "mine:", err)
		os.Exit(1)
	}
	fmt.Printf("mine: %d cases from %s written to %s\n", len(cases), *name, *out)
}

type commit struct {
	sha, subject string
	pr           int
}

func git(repo string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func mine(repo, name, ref string, depth int) ([]bench.Case, error) {
	raw, err := git(repo, "log", ref, "--no-merges", "--format=%H %s", "-"+strconv.Itoa(depth))
	if err != nil {
		return nil, err
	}
	var commits []commit
	byPR := map[int]commit{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		sha, subject, _ := strings.Cut(line, " ")
		c := commit{sha: sha, subject: subject}
		if m := prNumber.FindStringSubmatch(subject); m != nil {
			c.pr, _ = strconv.Atoi(m[1])
			byPR[c.pr] = c
		}
		commits = append(commits, c)
	}
	// case id -> case, expectations appended as fixes point at the same PR
	cases := map[string]*bench.Case{}
	for _, fix := range commits {
		if !strings.HasPrefix(fix.subject, "fix") || skipSubject.MatchString(fix.subject) || fix.pr == 0 {
			continue
		}
		touched, err := changedLines(repo, fix.sha)
		if err != nil {
			return nil, err
		}
		for path, ranges := range touched {
			if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testdata/") || skipPath.MatchString(path) {
				continue
			}
			for _, r := range ranges {
				blamed, err := blame(repo, fix.sha+"^", path, r[0], r[1])
				if err != nil {
					continue // path absent before the fix: new file, nothing introduced it
				}
				for origSHA, lines := range blamed {
					intro, ok := findCommit(commits, origSHA)
					if !ok || intro.pr == 0 || intro.pr == fix.pr || strings.HasPrefix(intro.subject, "fix(go)") {
						continue
					}
					id := fmt.Sprintf("%s#%d", name, intro.pr)
					c := cases[id]
					if c == nil {
						parent, err := git(repo, "rev-parse", intro.sha+"^")
						if err != nil {
							continue
						}
						c = &bench.Case{
							ID: id, Repository: name, PR: intro.pr, Title: strings.TrimSpace(prNumber.ReplaceAllString(intro.subject, "")),
							Head: intro.sha, Base: strings.TrimSpace(string(parent)), Source: "mined",
						}
						cases[id] = c
					}
					c.Expected = append(c.Expected, bench.Expected{
						Path: path, Lines: [2]int{lines[0], lines[1]}, Must: true, Fix: fix.sha,
						Description: strings.TrimSpace(prNumber.ReplaceAllString(fix.subject, "")),
					})
				}
			}
		}
	}
	out := make([]bench.Case, 0, len(cases))
	for _, c := range cases {
		c.Expected = mergeExpected(c.Expected)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PR < out[j].PR })
	return out, nil
}

func findCommit(commits []commit, sha string) (commit, bool) {
	for _, c := range commits {
		if c.sha == sha {
			return c, true
		}
	}
	return commit{}, false
}

// changedLines returns, per path, the old-side line ranges a commit
// removed or replaced: the lines that existed before the fix and were
// wrong. Pure additions blame nothing and are skipped.
func changedLines(repo, sha string) (map[string][][2]int, error) {
	raw, err := git(repo, "show", "--format=", "--unified=0", "--no-color", sha)
	if err != nil {
		return nil, err
	}
	out := map[string][][2]int{}
	var path string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "--- a/"):
			path = strings.TrimPrefix(line, "--- a/")
		case strings.HasPrefix(line, "--- /dev/null"):
			path = ""
		case strings.HasPrefix(line, "@@"):
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil || path == "" {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if n == 0 {
				continue // pure addition
			}
			out[path] = append(out[path], [2]int{start, start + n - 1})
		}
	}
	return out, sc.Err()
}

// blame maps the commits that introduced lines from..to of path at rev to
// the original line range in that commit's version of the file.
func blame(repo, rev, path string, from, to int) (map[string][2]int, error) {
	raw, err := git(repo, "blame", "--porcelain", "-L", fmt.Sprintf("%d,%d", from, to), rev, "--", path)
	if err != nil {
		return nil, err
	}
	out := map[string][2]int{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || len(f[0]) != 40 {
			continue
		}
		orig, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		r, ok := out[f[0]]
		if !ok {
			out[f[0]] = [2]int{orig, orig}
			continue
		}
		out[f[0]] = [2]int{min(r[0], orig), max(r[1], orig)}
	}
	return out, nil
}

// mergeExpected joins overlapping ranges on the same path from the same fix.
func mergeExpected(in []bench.Expected) []bench.Expected {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Path != in[j].Path {
			return in[i].Path < in[j].Path
		}
		return in[i].Lines[0] < in[j].Lines[0]
	})
	var out []bench.Expected
	for _, e := range in {
		if n := len(out); n > 0 && out[n-1].Path == e.Path && out[n-1].Fix == e.Fix && e.Lines[0] <= out[n-1].Lines[1]+bench.Tolerance {
			out[n-1].Lines[1] = max(out[n-1].Lines[1], e.Lines[1])
			continue
		}
		out = append(out, e)
	}
	return out
}
