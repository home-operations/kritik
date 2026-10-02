package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/runner"
)

func TestSplitDropped(t *testing.T) {
	loose := review.Finding{Path: "a.go", Line: 3, Title: "off the diff"}
	other := review.Finding{Path: "b.go", Line: 9, Title: "also off"}
	drop := func(reason review.DropReason, f review.Finding) review.Dropped {
		return review.Dropped{Finding: f, Reason: reason}
	}
	tests := []struct {
		name           string
		dropped        []review.Dropped
		wantUnanchored []review.Finding
		wantNotes      []string
	}{
		{name: "nothing dropped"},
		{name: "an unanchored finding is returned without a note",
			dropped: []review.Dropped{drop(review.DropUnanchored, loose)}, wantUnanchored: []review.Finding{loose}},
		{name: "other reasons are counted in one sorted note",
			dropped:   []review.Dropped{drop(review.DropNoFix, loose), drop(review.DropIncomplete, other), drop(review.DropIncomplete, loose)},
			wantNotes: []string{"3 finding(s) were dropped (incomplete: 2, no_suggested_fix: 1)"}},
		{name: "unanchored and counted reasons both come back",
			dropped:        []review.Dropped{drop(review.DropBadSeverity, other), drop(review.DropUnanchored, loose)},
			wantUnanchored: []review.Finding{loose}, wantNotes: []string{"1 finding(s) were dropped (bad_severity: 1)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unanchored, notes := splitDropped(tt.dropped)
			if !reflect.DeepEqual(unanchored, tt.wantUnanchored) {
				t.Errorf("unanchored = %+v, want %+v", unanchored, tt.wantUnanchored)
			}
			if !slices.Equal(notes, tt.wantNotes) {
				t.Errorf("notes = %q, want %q", notes, tt.wantNotes)
			}
		})
	}
}

func TestMarkedInline(t *testing.T) {
	const login = "kritika[bot]"
	fp := review.Fingerprint(review.Finding{Path: "a.go", Title: "nil deref"})
	marked := func(id int64, author, fingerprint string, inReplyTo int64) forge.Comment {
		return forge.Comment{ID: id, Author: author, Body: "**nil deref**\n\n" + review.FindingMarker(fingerprint), InReplyTo: inReplyTo}
	}
	tests := []struct {
		name     string
		comments []forge.Comment
		want     map[string]int64
	}{
		{name: "the bot's root comment maps its fingerprint", comments: []forge.Comment{marked(5, login, fp, 0)}, want: map[string]int64{fp: 5}},
		{name: "a reply is ignored", comments: []forge.Comment{marked(6, login, fp, 5)}, want: map[string]int64{}},
		{name: "another author's planted marker is ignored", comments: []forge.Comment{marked(7, "mallory", fp, 0)}, want: map[string]int64{}},
		{name: "the author match ignores case", comments: []forge.Comment{marked(8, "Kritika[Bot]", fp, 0)}, want: map[string]int64{fp: 8}},
		{name: "a later comment for the same finding wins",
			comments: []forge.Comment{marked(9, login, fp, 0), marked(10, login, fp, 0)}, want: map[string]int64{fp: 10}},
		{name: "a comment without a marker contributes nothing",
			comments: []forge.Comment{{ID: 11, Author: login, Body: "looks fine"}}, want: map[string]int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markedInline(tt.comments, login); !maps.Equal(got, tt.want) {
				t.Errorf("markedInline = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSkipDescription(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "the runner's unchanged patch", reason: runner.SkipUnchangedPatch, want: "patch unchanged since the last review"},
		{name: "the runner's too large diff", reason: runner.SkipTooLarge, want: "more than 800 changed lines"},
		{name: "a repository skip reason", reason: string(repoconfig.SkipOnlyPaths), want: repoconfig.SkipOnlyPaths.Description()},
		{name: "disabled", reason: string(repoconfig.SkipDisabled), want: "disabled in " + repoconfig.FileName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipDescription(tt.reason, 800); got != tt.want {
				t.Errorf("skipDescription(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

func TestResolvedThreads(t *testing.T) {
	prior := []priorFinding{
		{Path: "a.go", Title: "Unchecked error", commentID: 11},
		{Path: "b.go", Title: "Stale comment", commentID: 12},
		{Path: "c.go", Title: "Never posted inline"},
	}
	tests := []struct {
		name     string
		reported []review.Finding
		want     []int64
	}{
		{name: "every finding gone resolves every posted thread", want: []int64{11, 12}},
		{name: "a finding reported again keeps its thread", reported: []review.Finding{{Path: "a.go", Title: "unchecked  ERROR"}}, want: []int64{12}},
		{name: "a different finding on the same path resolves nothing of it", reported: []review.Finding{{Path: "a.go", Title: "Other"}, {Path: "b.go", Title: "Stale comment"}}, want: []int64{11}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvedThreads(review.Result{Findings: tt.reported}, prior)
			if !slices.Equal(got, tt.want) {
				t.Errorf("resolvedThreads() = %v, want %v", got, tt.want)
			}
		})
	}
}

// stickyForge keeps a pull request's conversation comments by id and
// records the calls a sticky comment write makes.
type stickyForge struct {
	forge.Client
	comments map[int64]string
	// updateErr fails every edit, over the missing-comment error.
	updateErr error
	calls     []string
}

func (f *stickyForge) BotLogin(context.Context) (string, error) { return "kritika[bot]", nil }

func (f *stickyForge) FindComment(_ context.Context, _, _ string, _ int, _, marker string) (int64, error) {
	f.calls = append(f.calls, "find")
	for _, id := range slices.Sorted(maps.Keys(f.comments)) {
		if f.comments[id] == marker {
			return id, nil
		}
	}
	return 0, nil
}

func (f *stickyForge) CreateComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	f.calls = append(f.calls, "create")
	f.comments[200] = body
	return 200, nil
}

func (f *stickyForge) UpdateComment(_ context.Context, _, _ string, id int64, body string) error {
	f.calls = append(f.calls, fmt.Sprintf("update %d", id))
	if f.updateErr != nil {
		return f.updateErr
	}
	if _, ok := f.comments[id]; !ok {
		return fmt.Errorf("fake: comment %d: %w", id, fs.ErrNotExist)
	}
	f.comments[id] = body
	return nil
}

func TestWriteSticky(t *testing.T) {
	t.Parallel()
	const marker = "<!-- kritika:pr-7 -->"
	tests := []struct {
		name      string
		stored    int64
		comments  map[int64]string
		updateErr error
		want      int64
		wantErr   error
		calls     []string
	}{
		{name: "the stored comment is edited", stored: 100, comments: map[int64]string{100: marker},
			want: 100, calls: []string{"update 100"}},
		{name: "a stored comment the forge no longer has is replaced", stored: 100, comments: map[int64]string{},
			want: 200, calls: []string{"update 100", "find", "create"}},
		{name: "a stored comment the forge no longer has yields to one by its marker", stored: 100, comments: map[int64]string{150: marker},
			want: 150, calls: []string{"update 100", "find", "update 150"}},
		{name: "another error editing the stored comment fails", stored: 100, comments: map[int64]string{100: marker},
			updateErr: errForge, wantErr: errForge, calls: []string{"update 100"}},
		{name: "nothing stored finds the comment by its marker", comments: map[int64]string{150: marker},
			want: 150, calls: []string{"find", "update 150"}},
		{name: "nothing stored and none on the forge creates one", comments: map[int64]string{},
			want: 200, calls: []string{"find", "create"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &stickyForge{comments: tt.comments, updateErr: tt.updateErr}
			p := &publishPhase{client: f, pr: &pullRequest{repository: "o/r", number: 7}, logger: slog.New(slog.DiscardHandler)}
			got, err := p.writeSticky(t.Context(), tt.stored, "body")
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("writeSticky(%d) = %d, %v; want %d, %v", tt.stored, got, err, tt.want, tt.wantErr)
			}
			if !slices.Equal(f.calls, tt.calls) {
				t.Errorf("forge calls = %v, want %v", f.calls, tt.calls)
			}
			if err == nil && f.comments[got] != "body" {
				t.Errorf("comment %d = %q, want the body written", got, f.comments[got])
			}
		})
	}
}

var errForge = errors.New("forge: forbidden")
