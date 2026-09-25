package agent

import (
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// binaryContent has a NUL byte inside the first 8 KiB, so it must be
// detected as binary regardless of its total length.
var binaryContent = "\x00\x01\x02binary stuff"

// testTree builds a small commit in memory and returns its tree: a Go
// file, a file matching an ignore glob, and a binary file.
func testTree(t *testing.T) *object.Tree {
	t.Helper()
	fs := memfs.New()
	r, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		f, err := fs.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n")
	write("widget.go", "package main\n\n// this function is the marker grep looks for.\nfunc findMe() {}\n\nfunc other() {}\n")
	write("generated/gen.go", "package generated\n\nfunc findMe() {}\n")
	write("image.png", binaryContent)
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	hash, err := wt.Commit("initial", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(hash)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestCleanPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{"simple", "main.go", "main.go", false},
		{"nested", "a/b/c.go", "a/b/c.go", false},
		{"empty", "", "", true},
		{"dot", ".", "", true},
		{"absolute", "/etc/passwd", "", true},
		{"dotdot", "..", "", true},
		{"escapes", "../etc/passwd", "", true},
		{"escapes-nested", "a/../../etc/passwd", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cleanPath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cleanPath(%q) = %q, nil; want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("cleanPath(%q) unexpected error: %v", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("cleanPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestIsBinary(t *testing.T) {
	if !isBinary(binaryContent) {
		t.Fatal("expected binary content to be detected as binary")
	}
	if isBinary("package main\n\nfunc main() {}\n") {
		t.Fatal("expected text content to not be detected as binary")
	}
}

func TestTreeFile(t *testing.T) {
	tree := testTree(t)
	nt := NewTree(tree, nil)

	if _, _, err := nt.file("does/not/exist.go"); err == nil {
		t.Fatal("expected error for missing file")
	}
	if _, _, err := nt.file("../escape.go"); err == nil {
		t.Fatal("expected error for escaping path")
	}
	f, cleaned, err := nt.file("main.go")
	if err != nil {
		t.Fatalf("file(main.go): %v", err)
	}
	if cleaned != "main.go" {
		t.Fatalf("cleaned = %q, want main.go", cleaned)
	}
	if f.Name != "main.go" {
		t.Fatalf("f.Name = %q, want main.go", f.Name)
	}
}

func TestTreeIgnored(t *testing.T) {
	nt := NewTree(testTree(t), []string{"generated/**"})
	if !nt.ignored("generated/gen.go") {
		t.Fatal("expected generated/gen.go to be ignored")
	}
	if nt.ignored("main.go") {
		t.Fatal("expected main.go to not be ignored")
	}
}
