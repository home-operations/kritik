package repoconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestAgentDirs(t *testing.T) {
	got := agentDirs([]string{"svc/api/v1/h.go", "main.go", "svc/api/x.go", "docs/a.md"})
	want := []string{"", "docs", "svc", "svc/api", "svc/api/v1"}
	if !slices.Equal(got, want) {
		t.Fatalf("agentDirs = %q, want %q", got, want)
	}
}

func TestReadAgentFiles(t *testing.T) {
	repo := map[string]string{
		"AGENTS.md": "root", "svc/CLAUDE.md": "svc", "svc/api/AGENTS.md": strings.Repeat("x", MaxFileBytes+1),
		"docs/AGENTS.md": "docs", "broken/AGENTS.md": "",
	}
	var reads []string
	read := func(p string) ([]byte, error) {
		reads = append(reads, p)
		if p == "broken/AGENTS.md" {
			return nil, errors.New("disk on fire")
		}
		c, ok := repo[p]
		if !ok {
			return nil, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		return []byte(c), nil
	}
	// AGENTS.md was already read as a named instruction file.
	files := Files{"AGENTS.md": "root"}
	notes, err := ReadAgentFiles(read, files, []string{"svc/api/h.go", "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Files{"AGENTS.md": "root", "svc/CLAUDE.md": "svc"}); !maps.Equal(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "svc/api/AGENTS.md: skipped") {
		t.Fatalf("notes = %q, want the oversized svc/api/AGENTS.md", notes)
	}
	if want := []string{"svc/AGENTS.md", "svc/CLAUDE.md", "svc/api/AGENTS.md"}; !slices.Equal(reads, want) {
		t.Fatalf("reads = %q, want %q: the root's was already read, and svc/api has an AGENTS.md", reads, want)
	}
	if _, err := ReadAgentFiles(read, Files{}, []string{"broken/x.go"}); err == nil {
		t.Fatal("a read error other than a missing file is returned")
	}
}

// TestAgentFiles: each changed directory's agent file, shallowest first,
// its CLAUDE.md only where it has no AGENTS.md, and none of a directory
// the change does not touch.
func TestAgentFiles(t *testing.T) {
	files := Files{"docs/review.md": "r", "AGENTS.md": "root", "CLAUDE.md": "@AGENTS.md", "svc/CLAUDE.md": "svc", "other/AGENTS.md": "other"}
	if got, want := AgentFiles(files, []string{"svc/h.go"}), []string{"AGENTS.md", "svc/CLAUDE.md"}; !slices.Equal(got, want) {
		t.Fatalf("AgentFiles = %q, want %q", got, want)
	}
}
