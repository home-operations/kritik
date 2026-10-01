package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"testing"

	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/review"
)

// issueForge answers Issue from a fixed set and fails the numbers it is
// told to.
type issueForge struct {
	forge.Client
	issues  map[int]forge.Issue
	failing int
	asked   []int
}

func (f *issueForge) Issue(_ context.Context, _, _ string, number int) (forge.Issue, error) {
	f.asked = append(f.asked, number)
	if number == f.failing {
		return forge.Issue{}, errors.New("github: 403 Resource not accessible by integration")
	}
	is, ok := f.issues[number]
	if !ok {
		return forge.Issue{}, fmt.Errorf("github: issue %d: %w", number, fs.ErrNotExist)
	}
	return is, nil
}

func TestLinkedIssues(t *testing.T) {
	client := &issueForge{issues: map[int]forge.Issue{
		12: {Number: 12, Title: "Widgets leak", Body: "Steps.", URL: "https://github.com/o/r/issues/12"},
		13: {Number: 13, Title: "A pull request", PullRequest: true},
	}, failing: 15}
	issues, notes := linkedIssues(t.Context(), client, "o", "r", "Closes #12, closes #13, fixes #14, resolves #15, and #16 later", slog.New(slog.DiscardHandler))
	want := []review.Issue{{Number: 12, Title: "Widgets leak", Body: "Steps."}}
	if !slices.Equal(issues, want) {
		t.Fatalf("issues = %+v, want %+v", issues, want)
	}
	if !slices.Equal(client.asked, []int{12, 13, 14}) {
		t.Fatalf("asked the forge for %v, want the first %d linked issues", client.asked, review.MaxLinkedIssues)
	}
	if len(notes) != 0 {
		t.Fatalf("notes = %q, want none for issues within the first three", notes)
	}
	_, notes = linkedIssues(t.Context(), client, "o", "r", "Fixes #15", slog.New(slog.DiscardHandler))
	if len(notes) != 1 || notes[0] != "issue #15, which the description says this closes, could not be read" {
		t.Fatalf("notes = %q, want the unreadable issue noted", notes)
	}
}
