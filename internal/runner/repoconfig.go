package runner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/home-operations/kritik/internal/repoconfig"
)

// repoConfig reads .kritik.yaml and the files it and the operator (extra)
// name from the merge-base tree, and returns the ignore globs with the
// file's own unioned in. A file that does not parse adds no globs; the
// worker reports why when it reads the same file back.
func repoConfig(base *object.Tree, ignore, extra []string) (repoconfig.Files, []string, []string, error) {
	files, notes, err := repoconfig.Collect(treeReader(base), extra...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("runner: %w", err)
	}
	m, _ := repoconfig.Merge(files, repoconfig.Operator{Ignore: ignore})
	return files, notes, m.Ignore, nil
}

// treeReader reads a blob by path. Anything that is not a file in the tree
// is fs.ErrNotExist, and a blob is read no further than one byte past the
// per-file cap, so an oversized file costs no more memory than a kept one.
func treeReader(tree *object.Tree) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		e, err := tree.FindEntry(name)
		if err != nil || !e.Mode.IsFile() {
			return nil, fmt.Errorf("runner: %s: %w", name, fs.ErrNotExist)
		}
		f, err := tree.File(name)
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, fmt.Errorf("runner: %s: %w", name, fs.ErrNotExist)
		}
		if err != nil {
			return nil, fmt.Errorf("runner: read %s: %w", name, err)
		}
		r, err := f.Reader()
		if err != nil {
			return nil, fmt.Errorf("runner: read %s: %w", name, err)
		}
		defer func() { _ = r.Close() }()
		b, err := io.ReadAll(io.LimitReader(r, repoconfig.MaxFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("runner: read %s: %w", name, err)
		}
		return b, nil
	}
}
