// Package agent is a bounded, read-only tool loop over a git commit's
// tree: a Stepper reads it through read_file, grep and list_files, then
// must call submit_review to end the run.
package agent

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// binarySniffBytes is how many bytes at the start of a file are checked for
// a NUL byte to decide it is binary.
const binarySniffBytes = 8 << 10

// Tree is a read-only view over a commit's tree for the agent's tools. The
// tools read this tree's git objects directly, never a working tree, so
// there is no path or symlink escape.
type Tree struct {
	root   *object.Tree
	ignore []string
}

// NewTree wraps t. ignore is a set of doublestar globs that grep and
// list_files skip; read_file may still read an ignored path.
func NewTree(t *object.Tree, ignore []string) *Tree {
	return &Tree{root: t, ignore: ignore}
}

// ignored reports whether p matches one of the tree's ignore globs.
func (t *Tree) ignored(p string) bool {
	for _, g := range t.ignore {
		if ok, _ := doublestar.Match(g, p); ok {
			return true
		}
	}
	return false
}

// file returns the tree's file at p, with p cleaned and validated first.
// The returned path is the cleaned form of p.
func (t *Tree) file(p string) (*object.File, string, error) {
	cleaned, err := cleanPath(p)
	if err != nil {
		return nil, "", err
	}
	f, err := t.root.File(cleaned)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", cleaned, err)
	}
	return f, cleaned, nil
}

// cleanPath rejects an empty, absolute, or tree-escaping path, and returns
// the cleaned, slash-separated form otherwise.
func cleanPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is empty")
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("path %q is absolute", p)
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "", fmt.Errorf("path %q is empty", p)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the tree", p)
	}
	return cleaned, nil
}

// isBinary reports whether content has a NUL byte in its first 8 KiB, the
// same heuristic git itself uses to skip a file in a text diff.
func isBinary(content string) bool {
	n := len(content)
	if n > binarySniffBytes {
		n = binarySniffBytes
	}
	return strings.IndexByte(content[:n], 0) >= 0
}

// splitLines splits content into lines, stripping a trailing empty line
// left by a final newline so a file's line count matches what an editor
// would show.
func splitLines(content string) []string {
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		return lines[:n-1]
	}
	return lines
}
