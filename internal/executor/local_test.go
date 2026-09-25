package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalMasksTheRunError(t *testing.T) {
	const token = "ghs_secret_token"
	s := spec()
	s.Deadline = 0
	// An invalid head makes the runner's own error quote it, before the
	// runner touches the store.
	s.Job.Head = token
	res := (&Local{}).Run(t.Context(), s)
	if res.Err == nil || strings.Contains(res.Err.Error(), token) || !strings.Contains(res.Err.Error(), "***") {
		t.Fatalf("err = %v", res.Err)
	}
}

func TestMaskedErrorUnwraps(t *testing.T) {
	err := maskedError{msg: "runner: ***", err: context.DeadlineExceeded}
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "runner: ***" {
		t.Fatalf("err = %v", err)
	}
}

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
