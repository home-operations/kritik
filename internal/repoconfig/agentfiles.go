package repoconfig

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// agentFileNames are the files a directory may give coding agents, in the
// order one is looked for: CLAUDE.md is read only where there is no
// AGENTS.md, which it commonly just imports.
var agentFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// agentDirs is the repository root and every directory holding a changed
// path or one of its parents, shallowest first.
func agentDirs(changed []string) []string {
	seen := map[string]bool{"": true}
	for _, c := range changed {
		for d := path.Dir(c); d != "." && !seen[d]; d = path.Dir(d) {
			seen[d] = true
		}
	}
	depth := func(d string) int {
		if d == "" {
			return -1
		}
		return strings.Count(d, "/")
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	slices.SortFunc(dirs, func(a, b string) int { return cmp.Or(cmp.Compare(depth(a), depth(b)), strings.Compare(a, b)) })
	return dirs
}

// ReadAgentFiles adds to files, through read as Collect takes it, each
// agent file of the directories agentDirs gives for changed: a
// directory's AGENTS.md, or its CLAUDE.md when it has none. A directory
// with neither is not noted; a file over MaxFileBytes, or one that would
// push the total over MaxTotalBytes, is left out and noted.
func ReadAgentFiles(read func(name string) ([]byte, error), files Files, changed []string) ([]string, error) {
	var total int
	for _, c := range files {
		total += len(c)
	}
	var notes []string
	for _, dir := range agentDirs(changed) {
		for _, name := range agentFileNames {
			p := path.Join(dir, name)
			if _, seen := files[p]; seen {
				break
			}
			b, err := read(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			switch {
			case err != nil:
				return nil, fmt.Errorf("repoconfig: read %s: %w", p, err)
			case len(b) > MaxFileBytes:
				notes = append(notes, TooLarge(p))
			case total+len(b) > MaxTotalBytes:
				notes = append(notes, overTotal(p))
			default:
				files[p] = string(b)
				total += len(b)
			}
			break
		}
	}
	return notes, nil
}

// AgentFiles is the agent files of files that apply to changed, as
// ReadAgentFiles picks them, shallowest first: the repository's
// instructions.
func AgentFiles(files Files, changed []string) []string {
	var out []string
	for _, dir := range agentDirs(changed) {
		for _, name := range agentFileNames {
			p := path.Join(dir, name)
			if _, ok := files[p]; ok {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
