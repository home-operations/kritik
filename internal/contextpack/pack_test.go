package contextpack

import (
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

const widgetBase = `package demo

// Widget is a thing.
type Widget struct {
	Name string
}

func Build(name string) *Widget {
	if name == "" {
		name = "default"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	w := &Widget{Name: name}
	if w.Name == "skip" {
		return nil
	}
	return w
}
`

const widgetHead = `package demo

// Widget is a thing.
type Widget struct {
	Name string
}

func Build(name string) *Widget {
	if name == "" {
		name = "default"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	w := &Widget{Name: Normalize(name)}
	if w.Name == "skip" {
		return nil
	}
	return w
}
`

const useSource = `package demo

import "fmt"

func Normalize(s string) string {
	return s
}

func Use() {
	fmt.Println(Build("x"))
}

func Unrelated() {}
`

// repo builds base and head commits in memory and returns their trees and
// the diff between them.
func repo(t *testing.T) (head, base *object.Tree, diff string) {
	t.Helper()
	fs := memfs.New()
	r, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	write := func(name, content string) {
		f, _ := fs.Create(name)
		_, _ = f.Write([]byte(content))
		_ = f.Close()
		_, _ = wt.Add(name)
	}
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	write("widget.go", widgetBase)
	write("use.go", useSource)
	write("vendor/dep.go", "package dep\n\nfunc Build() {}\n")
	write("values.yaml", "image: 1\n")
	baseHash, _ := wt.Commit("base", &git.CommitOptions{Author: sig})
	write("widget.go", widgetHead)
	headHash, _ := wt.Commit("head", &git.CommitOptions{Author: sig})
	bc, _ := r.CommitObject(baseHash)
	hc, _ := r.CommitObject(headHash)
	base, _ = bc.Tree()
	head, _ = hc.Tree()
	patch, _ := bc.Patch(hc)
	return head, base, patch.String()
}

func TestBuildStages(t *testing.T) {
	head, base, diff := repo(t)
	chunks, stats, err := Build(t.Context(), Input{Head: head, Base: base, Diff: diff, Changed: []string{"widget.go"}, Ignore: []string{"vendor/**"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	byStage := map[string][]Chunk{}
	for _, c := range chunks {
		byStage[c.Stage] = append(byStage[c.Stage], c)
	}
	ov := byStage[StageOverlay]
	if len(ov) != 1 || ov[0].Symbol != "Build" || ov[0].Kind != "function" || !strings.Contains(ov[0].Text, "Normalize(name)") || !strings.Contains(ov[0].Text, "return w") || ov[0].Path != "widget.go" {
		t.Fatalf("overlay = %+v", ov)
	}
	checkDefinitions(t, byStage[StageDefinition])
	callers := byStage[StageCaller]
	if len(callers) != 1 || callers[0].Symbol != "Use" || callers[0].Ref != "Build" || callers[0].Path != "use.go" {
		t.Fatalf("callers = %+v; want Use in use.go", callers)
	}
	if stats.FilesScanned != 2 || stats.FilesParsed != 2 || stats.Overlay != 1 || stats.Callers != 1 || stats.ScanTruncated {
		t.Fatalf("stats = %+v", stats)
	}
	for _, c := range chunks {
		if c.Text == "" || c.StartLine == 0 || c.EndLine < c.StartLine {
			t.Fatalf("bad chunk %+v", c)
		}
	}
}

func checkDefinitions(t *testing.T, defs []Chunk) {
	t.Helper()
	var normalize, widget bool
	for _, d := range defs {
		switch {
		case d.Symbol == "Normalize" && d.Path == "use.go" && d.Ref == "Normalize":
			normalize = true
		case d.Symbol == "Widget" && d.Path == "widget.go" && d.Kind == "type":
			widget = true
		case d.Path == "vendor/dep.go":
			t.Fatalf("ignored path leaked into definitions: %+v", d)
		}
	}
	if !normalize || !widget {
		t.Fatalf("definitions = %+v; want Normalize (other file) and Widget (same file)", defs)
	}
}

func TestBuildWindowsConfigFiles(t *testing.T) {
	head, base, _ := repo(t)
	diff := "diff --git a/values.yaml b/values.yaml\n--- a/values.yaml\n+++ b/values.yaml\n@@ -1 +1 @@\n-image: 1\n+image: 2\n"
	chunks, _, err := Build(t.Context(), Input{Head: head, Base: base, Diff: diff, Changed: []string{"values.yaml"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A one-line file is fully shown by the diff, so the window adds nothing.
	if len(chunks) != 0 {
		t.Fatalf("chunks = %+v; want none", chunks)
	}
}

func TestParseDiff(t *testing.T) {
	d := parseDiff("diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -3,4 +3,5 @@\n a\n-b\n+B\n+C\n d\n e\n")
	if got := d.added["x.go"]; len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("added = %v", got)
	}
	if got := d.removed["x.go"]; len(got) != 1 || got[0] != 4 {
		t.Fatalf("removed = %v", got)
	}
	if !d.shown["x.go"][3] || !d.shown["x.go"][7] || d.shown["x.go"][8] {
		t.Fatalf("shown = %v", d.shown["x.go"])
	}
	if r := runs([]int{1, 2, 3, 10, 30, 31}, 5); len(r) != 3 || r[0] != [2]int{1, 3} || r[1] != [2]int{10, 10} || r[2] != [2]int{30, 31} {
		t.Fatalf("runs = %v", r)
	}
}

func TestHunks(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -3,4 +3,5 @@\n a\n-b\n+B\n+C\n d\n@@ -20,2 +21,2 @@\n-x\n+y\n z\n" +
		"diff --git a/gone b/gone\n--- a/gone\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n"
	h := Hunks(diff)
	if len(h) != 2 || h[0].Path != "x.go" || h[0].Text != "a\nB\nC\nd\n" || h[1].Text != "y\nz\n" {
		t.Fatalf("hunks = %+v", h)
	}
}

func TestPickPrefersDistinctFiles(t *testing.T) {
	cs := []Chunk{{Path: "a", StartLine: 1}, {Path: "a", StartLine: 10}, {Path: "b", StartLine: 1}}
	got := pick(cs, 2)
	if len(got) != 2 || got[0].Path != "a" || got[1].Path != "b" {
		t.Fatalf("pick = %+v", got)
	}
	if got := pick(cs, 5); len(got) != 3 {
		t.Fatalf("pick under the cap must keep everything, got %d", len(got))
	}
}
