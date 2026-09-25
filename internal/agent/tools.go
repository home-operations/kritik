package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/home-operations/kritik/internal/model"
)

// defaultGrepMaxResults and grepMaxResultsCap bound grep's max_results
// input: unset (or non-positive) falls back to the default, and any value
// above the cap is clamped to it.
const (
	defaultGrepMaxResults = 100
	grepMaxResultsCap     = 500
)

// truncate caps s at max bytes, appending a note of how much was cut. A
// non-positive max disables truncation.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n[truncated %d bytes]", len(s)-max)
}

// decodeInput unmarshals input into v, treating a nil or empty input as
// "no fields set" rather than a JSON error.
func decodeInput(input json.RawMessage, v any) error {
	if len(input) == 0 {
		return nil
	}
	return json.Unmarshal(input, v)
}

var readFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {"type": "string", "description": "File path relative to the repository root."},
		"start_line": {"type": "integer", "description": "First line to include, 1-based; defaults to 1."},
		"end_line": {"type": "integer", "description": "Last line to include, 1-based inclusive; defaults to the end of the file."}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

type readFileTool struct {
	tree     *Tree
	maxBytes int
}

// ReadFileTool reads a file from t, optionally a 1-based inclusive line
// range, as lines numbered "N\t...". Its output is capped at maxBytes.
func ReadFileTool(t *Tree, maxBytes int) Tool {
	return &readFileTool{tree: t, maxBytes: maxBytes}
}

func (rt *readFileTool) Def() model.ToolDef {
	return model.ToolDef{
		Name:        "read_file",
		Description: "Read a file from the repository at the reviewed commit, optionally a line range.",
		InputSchema: readFileSchema,
	}
}

func (rt *readFileTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	var req struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := decodeInput(input, &req); err != nil {
		return "", fmt.Errorf("agent: read_file: %w", err)
	}

	f, cleaned, err := rt.tree.file(req.Path)
	if err != nil {
		return "", fmt.Errorf("agent: read_file: %w", err)
	}
	content, err := f.Contents()
	if err != nil {
		return "", fmt.Errorf("agent: read_file: %s: %w", cleaned, err)
	}
	if isBinary(content) {
		return "", fmt.Errorf("agent: read_file: %s is binary", cleaned)
	}
	lines := splitLines(content)

	start, end := req.StartLine, req.EndLine
	if start == 0 {
		start = 1
	}
	if end == 0 {
		end = len(lines)
	}
	if start < 1 {
		return "", fmt.Errorf("agent: read_file: %s: start_line must be >= 1", cleaned)
	}
	if end < start {
		return "", fmt.Errorf("agent: read_file: %s: end_line must be >= start_line", cleaned)
	}
	if start > len(lines) {
		return "", fmt.Errorf("agent: read_file: %s: start_line %d is beyond the file's %d lines", cleaned, start, len(lines))
	}
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := start; i <= end; i++ {
		if i > start {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", i, lines[i-1])
	}
	return truncate(b.String(), rt.maxBytes), nil
}

var grepSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {"type": "string", "description": "RE2 regular expression to search for."},
		"path_glob": {"type": "string", "description": "Doublestar glob restricting which files are searched; defaults to \"**\"."},
		"max_results": {"type": "integer", "description": "Maximum matches to return; defaults to 100, capped at 500."}
	},
	"required": ["pattern"],
	"additionalProperties": false
}`)

type grepTool struct {
	tree     *Tree
	maxBytes int
}

// GrepTool searches every non-binary, non-ignored file in t whose path
// matches path_glob for lines matching an RE2 pattern. Its output is capped
// at maxBytes.
func GrepTool(t *Tree, maxBytes int) Tool {
	return &grepTool{tree: t, maxBytes: maxBytes}
}

func (gt *grepTool) Def() model.ToolDef {
	return model.ToolDef{
		Name:        "grep",
		Description: "Search the repository at the reviewed commit for lines matching a regular expression.",
		InputSchema: grepSchema,
	}
}

type grepMatch struct {
	path string
	line int
	text string
}

func (gt *grepTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var req struct {
		Pattern    string `json:"pattern"`
		PathGlob   string `json:"path_glob"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeInput(input, &req); err != nil {
		return "", fmt.Errorf("agent: grep: %w", err)
	}
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		return "", fmt.Errorf("agent: grep: invalid pattern: %w", err)
	}
	glob := req.PathGlob
	if glob == "" {
		glob = "**"
	}
	max := req.MaxResults
	if max <= 0 {
		max = defaultGrepMaxResults
	} else if max > grepMaxResultsCap {
		max = grepMaxResultsCap
	}

	var matches []grepMatch
	iter := gt.tree.root.Files()
	err = iter.ForEach(func(f *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if gt.tree.ignored(f.Name) {
			return nil
		}
		if ok, _ := doublestar.Match(glob, f.Name); !ok {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		if isBinary(content) {
			return nil
		}
		for i, line := range splitLines(content) {
			if re.MatchString(line) {
				matches = append(matches, grepMatch{path: f.Name, line: i + 1, text: line})
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("agent: grep: %w", err)
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].path != matches[j].path {
			return matches[i].path < matches[j].path
		}
		return matches[i].line < matches[j].line
	})
	if len(matches) > max {
		matches = matches[:max]
	}

	lines := make([]string, len(matches))
	for i, m := range matches {
		lines[i] = fmt.Sprintf("%s:%d: %s", m.path, m.line, m.text)
	}
	return truncate(strings.Join(lines, "\n"), gt.maxBytes), nil
}

var listFilesSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"glob": {"type": "string", "description": "Doublestar glob to list; defaults to \"**\" (every file)."}
	},
	"additionalProperties": false
}`)

type listFilesTool struct {
	tree     *Tree
	maxBytes int
}

// ListFilesTool lists every non-ignored path in t matching glob. Its output
// is capped at maxBytes.
func ListFilesTool(t *Tree, maxBytes int) Tool {
	return &listFilesTool{tree: t, maxBytes: maxBytes}
}

func (lt *listFilesTool) Def() model.ToolDef {
	return model.ToolDef{
		Name:        "list_files",
		Description: "List file paths in the repository at the reviewed commit matching a glob.",
		InputSchema: listFilesSchema,
	}
}

func (lt *listFilesTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var req struct {
		Glob string `json:"glob"`
	}
	if err := decodeInput(input, &req); err != nil {
		return "", fmt.Errorf("agent: list_files: %w", err)
	}
	glob := req.Glob
	if glob == "" {
		glob = "**"
	}

	var paths []string
	iter := lt.tree.root.Files()
	err := iter.ForEach(func(f *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lt.tree.ignored(f.Name) {
			return nil
		}
		if ok, _ := doublestar.Match(glob, f.Name); !ok {
			return nil
		}
		paths = append(paths, f.Name)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("agent: list_files: %w", err)
	}

	sort.Strings(paths)
	return truncate(strings.Join(paths, "\n"), lt.maxBytes), nil
}
