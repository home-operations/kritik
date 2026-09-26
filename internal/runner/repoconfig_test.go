package runner

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/home-operations/kritik/internal/repoconfig"
)

func tree(t *testing.T, files map[string]string) *object.Tree {
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
	for name, content := range files {
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
	h, err := wt.Commit("c", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(h)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestRepoFiles(t *testing.T) {
	big := strings.Repeat("x", repoconfig.MaxFileBytes+1)
	tests := []struct {
		name      string
		files     map[string]string
		paths     []string
		wantFiles []string
		wantNotes int
	}{
		{name: "nothing named", files: map[string]string{"main.go": "package main\n"}},
		{
			name:      "the named files are read, and nothing else",
			files:     map[string]string{repoconfig.FileName: "review: {}\n", "docs/rules.md": "rules", "docs/other.md": "other"},
			paths:     []string{repoconfig.FileName, "docs/rules.md"},
			wantFiles: []string{repoconfig.FileName, "docs/rules.md"},
		},
		{
			name:      "a directory, a missing path and an oversized file are noted",
			files:     map[string]string{"docs/a.md": "a", "big.md": big},
			paths:     []string{"docs", "gone.md", "big.md"},
			wantNotes: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, notes, err := repoFiles(tree(t, tt.files), tt.paths)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for p := range files {
				got = append(got, p)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantFiles) || len(notes) != tt.wantNotes {
				t.Fatalf("files=%v notes=%q", got, notes)
			}
		})
	}
}

func TestNotIgnored(t *testing.T) {
	cases := []struct {
		name          string
		paths, ignore []string
		want          []string
	}{
		{name: "nothing ignored", paths: []string{"main.go", "a/b.go"}, want: []string{"main.go", "a/b.go"}},
		{name: "vendored churn drops out", paths: []string{"main.go", "vendor/x/y.go", "vendor/z.go"}, ignore: []string{"vendor/**"},
			want: []string{"main.go"}},
		{name: "no paths", ignore: []string{"**"}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := notIgnored(tc.paths, tc.ignore)
			if got == nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("notIgnored = %#v, want %#v", got, tc.want)
			}
		})
	}
}
