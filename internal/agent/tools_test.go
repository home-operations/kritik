package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReadFileTool(t *testing.T) {
	tree := NewTree(testTree(t), nil)
	tool := ReadFileTool(tree, 1<<20)

	t.Run("whole file", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"path":"main.go"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "1\tpackage main") {
			t.Fatalf("out = %q", out)
		}
		if !strings.Contains(out, "5\t}") {
			t.Fatalf("out missing last numbered line: %q", out)
		}
	})

	t.Run("range", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"path":"main.go","start_line":4,"end_line":4}`))
		if err != nil {
			t.Fatal(err)
		}
		if out != "4\t\tprintln(\"hi\")" {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := tool.Run(t.Context(), json.RawMessage(`{"path":"nope.go"}`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("dotdot rejected", func(t *testing.T) {
		if _, err := tool.Run(t.Context(), json.RawMessage(`{"path":"../etc/passwd"}`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("binary rejected", func(t *testing.T) {
		if _, err := tool.Run(t.Context(), json.RawMessage(`{"path":"image.png"}`)); err == nil {
			t.Fatal("expected error for binary file")
		}
	})

	t.Run("start beyond eof", func(t *testing.T) {
		if _, err := tool.Run(t.Context(), json.RawMessage(`{"path":"main.go","start_line":999}`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("ignored path still readable", func(t *testing.T) {
		ignoring := NewTree(testTree(t), []string{"generated/**"})
		out, err := ReadFileTool(ignoring, 1<<20).Run(t.Context(), json.RawMessage(`{"path":"generated/gen.go"}`))
		if err != nil {
			t.Fatalf("read_file must ignore the ignore list: %v", err)
		}
		if !strings.Contains(out, "findMe") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("truncation", func(t *testing.T) {
		small := ReadFileTool(tree, 5)
		out, err := small.Run(t.Context(), json.RawMessage(`{"path":"main.go"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "[truncated") {
			t.Fatalf("out = %q; want truncation marker", out)
		}
	})
}

func TestGrepTool(t *testing.T) {
	tree := NewTree(testTree(t), []string{"generated/**"})
	tool := GrepTool(tree, 1<<20)

	t.Run("matches across files sorted", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"findMe"}`))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(out, "\n")
		if len(lines) != 1 {
			t.Fatalf("out = %q; want exactly 1 match (generated/gen.go ignored)", out)
		}
		if !strings.HasPrefix(lines[0], "widget.go:") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("glob restricts search", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"package","path_glob":"main.go"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "main.go:1:") || strings.Contains(out, "widget.go") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("binary file skipped", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"binary"}`))
		if err != nil {
			t.Fatal(err)
		}
		if out != "" {
			t.Fatalf("out = %q; want no matches (binary file skipped)", out)
		}
	})

	t.Run("max_results caps and defaults", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"func","max_results":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.Split(out, "\n")) != 1 {
			t.Fatalf("out = %q; want exactly 1 line", out)
		}
	})

	t.Run("invalid pattern errors", func(t *testing.T) {
		if _, err := tool.Run(t.Context(), json.RawMessage(`{"pattern":"("}`)); err == nil {
			t.Fatal("expected error for invalid RE2 pattern")
		}
	})
}

func TestListFilesTool(t *testing.T) {
	tree := NewTree(testTree(t), []string{"generated/**"})
	tool := ListFilesTool(tree, 1<<20)

	t.Run("default glob excludes ignored", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "generated/gen.go") {
			t.Fatalf("out = %q; ignored path leaked", out)
		}
		if !strings.Contains(out, "main.go") || !strings.Contains(out, "widget.go") || !strings.Contains(out, "image.png") {
			t.Fatalf("out = %q; missing expected files", out)
		}
	})

	t.Run("glob filters", func(t *testing.T) {
		out, err := tool.Run(t.Context(), json.RawMessage(`{"glob":"*.png"}`))
		if err != nil {
			t.Fatal(err)
		}
		if out != "image.png" {
			t.Fatalf("out = %q", out)
		}
	})
}
