//go:build bench

// Package bench is the offline evaluation harness (ADR-0002 §2.14): a
// corpus of pull requests with known defects, run through the same fetch,
// context and prompt code the service uses, against a live model, with
// recall on must-find cases and cost per review as the output. It runs
// only locally with a model key, never in CI.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Case is one pull request with what a reviewer should have caught.
type Case struct {
	// ID is unique across files: "<repo>#<pr>".
	ID string `yaml:"id"`
	// Repository is owner/name; CloneURL defaults to GitHub's HTTPS URL.
	Repository string `yaml:"repository"`
	CloneURL   string `yaml:"cloneUrl,omitempty"`
	// PR is the pull request number, for the record.
	PR    int    `yaml:"pr"`
	Title string `yaml:"title"`
	// Head and Base are the two commits the review diffs: for a squash
	// merged PR, the squash commit and its parent.
	Head string `yaml:"head"`
	Base string `yaml:"base"`
	// Expected findings. A case with no must-find entry counts for
	// precision only.
	Expected []Expected `yaml:"expected"`
	// Source says how the case was made: mined (a later fix commit blamed
	// back to this PR) or manual.
	Source string `yaml:"source"`
	// Notes are free text for the curator.
	Notes string `yaml:"notes,omitempty"`
}

// Expected is one defect a reviewer should point at.
type Expected struct {
	Path string `yaml:"path"`
	// Lines is the inclusive head-side line range; a finding within
	// Tolerance lines of it matches.
	Lines [2]int `yaml:"lines"`
	// Description is a sentence for the curator; the harness never shows
	// it to the model.
	Description string `yaml:"description"`
	// Must marks a finding a review has to make; the rest are nice to have.
	Must bool `yaml:"must"`
	// Fix is the commit that fixed it, when mined.
	Fix string `yaml:"fix,omitempty"`
}

// Tolerance is how far, in lines, a finding may land from an expected
// range and still count.
const Tolerance = 3

// File is one corpus file.
type File struct {
	Cases []Case `yaml:"cases"`
}

// Load reads every corpus file matching glob, sorted by id.
func Load(glob string) ([]Case, error) {
	paths, err := filepath.Glob(glob)
	if err != nil {
		return nil, err
	}
	var out []Case
	seen := map[string]bool{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var f File
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("bench: %s: %w", p, err)
		}
		for _, c := range f.Cases {
			if c.ID == "" || c.Head == "" || c.Base == "" || c.Repository == "" {
				return nil, fmt.Errorf("bench: %s: case %q needs id, repository, head and base", p, c.ID)
			}
			if seen[c.ID] {
				return nil, fmt.Errorf("bench: duplicate case id %s", c.ID)
			}
			seen[c.ID] = true
			if c.CloneURL == "" {
				c.CloneURL = "https://github.com/" + c.Repository + ".git"
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Matches reports whether a finding at path:line lands on the expectation.
func (e Expected) Matches(path string, line int) bool {
	return path == e.Path && line >= e.Lines[0]-Tolerance && line <= e.Lines[1]+Tolerance
}
