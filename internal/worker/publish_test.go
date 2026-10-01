package worker

import (
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
		{name: "a repository skip reason", reason: string(repoconfig.SkipOnlyPaths), want: repoconfig.SkipOnlyPaths.Description()},
		{name: "disabled", reason: string(repoconfig.SkipDisabled), want: "disabled in " + repoconfig.FileName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipDescription(tt.reason); got != tt.want {
				t.Errorf("skipDescription(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}
