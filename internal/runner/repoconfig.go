package runner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/home-operations/kritik/internal/repoconfig"
)

// repoFiles reads the repository files the spec names from the merge-base
// tree, then the agent files the directories of agentChanged hold, noting
// any it could not keep.
func repoFiles(base *object.Tree, paths, agentChanged []string) (repoconfig.Files, []string, error) {
	files, notes, err := repoconfig.Collect(treeReader(base), paths...)
	if err != nil {
		return nil, nil, fmt.Errorf("runner: %w", err)
	}
	if len(agentChanged) > 0 {
		more, err := repoconfig.ReadAgentFiles(treeReader(base), files, agentChanged)
		if err != nil {
			return nil, nil, fmt.Errorf("runner: %w", err)
		}
		notes = append(notes, more...)
	}
	return files, notes, nil
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
