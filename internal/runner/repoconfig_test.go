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
	wt, _ := r.Worktree()
	for name, content := range files {
		f, _ := fs.Create(name)
		_, _ = f.Write([]byte(content))
		_ = f.Close()
		_, _ = wt.Add(name)
	}
	h, err := wt.Commit("c", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.CommitObject(h)
	tr, _ := c.Tree()
	return tr
}

func TestRepoConfig(t *testing.T) {
	big := strings.Repeat("x", repoconfig.MaxFileBytes+1)
	tests := []struct {
		name       string
		files      map[string]string
		extra      []string
		wantFiles  []string
		wantIgnore []string
		wantNotes  int
	}{
		{
			name:       "no file keeps the operator's ignore",
			files:      map[string]string{"main.go": "package main\n"},
			wantIgnore: []string{"vendor/**"},
		},
		{
			name: "in-repo ignore is unioned and referenced files are read",
			files: map[string]string{
				repoconfig.FileName: "ignore: [gen/**, vendor/**]\nreview:\n  instructions: [docs/rules.md]\n",
				"docs/rules.md":     "rules",
				"docs/extra.md":     "extra",
			},
			extra:      []string{"docs/extra.md"},
			wantFiles:  []string{repoconfig.FileName, "docs/extra.md", "docs/rules.md"},
			wantIgnore: []string{"vendor/**", "gen/**"},
		},
		{
			name:       "an invalid file contributes no ignore globs",
			files:      map[string]string{repoconfig.FileName: "ignore: [gen/**]\nunknown: 1\n"},
			wantFiles:  []string{repoconfig.FileName},
			wantIgnore: []string{"vendor/**"},
		},
		{
			name:       "a directory, a missing path and an oversized file are noted",
			files:      map[string]string{repoconfig.FileName: "review:\n  instructions: [docs, gone.md, big.md]\n", "docs/a.md": "a", "big.md": big},
			wantFiles:  []string{repoconfig.FileName},
			wantIgnore: []string{"vendor/**"},
			wantNotes:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, notes, ignore, err := repoConfig(tree(t, tt.files), []string{"vendor/**"}, tt.extra)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for p := range files {
				got = append(got, p)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantFiles) || !slices.Equal(ignore, tt.wantIgnore) || len(notes) != tt.wantNotes {
				t.Fatalf("files=%v ignore=%v notes=%q", got, ignore, notes)
			}
		})
	}
}
