package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTail(t *testing.T) {
	if tail("abcdef", 3) != "def" || tail("ab", 3) != "ab" {
		t.Fatal("tail")
	}
}

func TestNamespaceFromServiceAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("kritik\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ns, err := readNamespace(path)
	if err != nil || ns != "kritik" {
		t.Fatalf("readNamespace = %q, %v", ns, err)
	}
	if _, err := readNamespace(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "pod namespace") {
		t.Fatalf("missing file err = %v", err)
	}
}
