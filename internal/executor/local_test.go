package executor

import (
	"context"
	"errors"
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
