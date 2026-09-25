package indexer

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

const goFile = `package demo

type Widget struct{ Name string }

func Build(name string) *Widget { return &Widget{Name: name} }

func Use() { _ = Build("x") }
`

func trees(t *testing.T) (head, base *object.Tree) {
	t.Helper()
	fs := memfs.New()
	r, _ := git.Init(memory.NewStorage(), fs)
	wt, _ := r.Worktree()
	write := func(name, content string) {
		f, _ := fs.Create(name)
		_, _ = f.Write([]byte(content))
		_ = f.Close()
		_, _ = wt.Add(name)
	}
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	write("demo.go", goFile)
	write("values.yaml", strings.Repeat("key: value\n", 130))
	write("vendor/x.go", "package x\n\nfunc V() {}\n")
	write("gone.go", "package demo\n\nfunc Gone() {}\n")
	baseHash, _ := wt.Commit("base", &git.CommitOptions{Author: sig})
	write("demo.go", goFile+"\nfunc More() {}\n")
	_, _ = wt.Remove("gone.go")
	headHash, _ := wt.Commit("head", &git.CommitOptions{Author: sig})
	bc, _ := r.CommitObject(baseHash)
	hc, _ := r.CommitObject(headHash)
	base, _ = bc.Tree()
	head, _ = hc.Tree()
	return head, base
}

func TestBuildFull(t *testing.T) {
	head, _ := trees(t)
	chunks, changed, stats, err := Build(t.Context(), head, nil, []string{"vendor/**"}, Options{})
	if err != nil || changed != nil {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	var symbols []string
	windows := 0
	for _, c := range chunks {
		if c.Path == "vendor/x.go" {
			t.Fatalf("ignored path chunked: %+v", c)
		}
		if c.Kind == "window" {
			windows++
			if c.Path != "values.yaml" || c.Language != "yaml" {
				t.Fatalf("window from %+v", c)
			}
		}
		if c.Symbol != "" {
			symbols = append(symbols, c.Symbol)
		}
	}
	if windows != 3 {
		t.Fatalf("windows = %d, want 3 for 130 lines at 60/10", windows)
	}
	for _, want := range []string{"Widget", "Build", "Use", "More"} {
		if !contains(symbols, want) {
			t.Fatalf("symbols %v missing %s", symbols, want)
		}
	}
	if stats.Files != 2 || stats.Parsed != 1 || stats.Skipped < 1 || stats.Chunks != len(chunks) {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestBuildIncremental(t *testing.T) {
	head, base := trees(t)
	chunks, changed, _, err := Build(t.Context(), head, base, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || !contains(changed, "demo.go") || !contains(changed, "gone.go") {
		t.Fatalf("changed = %v", changed)
	}
	for _, c := range chunks {
		if c.Path != "demo.go" {
			t.Fatalf("unchanged path chunked: %+v", c)
		}
	}
	if len(chunks) < 4 {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestBuildStopsAtBudget(t *testing.T) {
	head, _ := trees(t)
	opts := DefaultOptions
	opts.MaxFiles = 1
	_, _, stats, err := Build(t.Context(), head, nil, nil, opts)
	if err != nil || !stats.Truncated || stats.Files != 1 {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
}

func contains(ss []string, s string) bool {
	return slices.Contains(ss, s)
}
