package worker

import (
	"reflect"
	"testing"

	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
)

func TestRequestsDismiss(t *testing.T) {
	for _, tt := range []struct {
		body   string
		reason string
		want   bool
	}{
		{"@kritika dismiss", "", true},
		{"@kritika dismiss: we lock this upstream", "we lock this upstream", true},
		{"@Kritika Dismissed, the caller checks nil\nalways has", "the caller checks nil\nalways has", true},
		{"@kritika dismissing this", "", false},
		{"@kritikabot dismiss", "", false},
		{"please @kritika review", "", false},
		{"dismiss @kritika", "", false},
	} {
		reason, ok := requestsDismiss(tt.body, "kritika")
		if ok != tt.want || reason != tt.reason {
			t.Errorf("requestsDismiss(%q) = %q, %v; want %q, %v", tt.body, reason, ok, tt.reason, tt.want)
		}
	}
}

func TestWithDismissals(t *testing.T) {
	kept := priorFinding{Path: "a.go", Title: "Unchecked error", commentID: 11}
	gone := priorFinding{Path: "b.go", Title: "Stale comment", commentID: 12}
	dismissed := []store.Dismissal{{Fingerprint: review.Fingerprint(gone.Finding), Finding: gone.Finding, Reason: "intended"}}
	p := priorReview{findings: []priorFinding{kept, gone}}
	p.withDismissals(dismissed)
	if len(p.findings) != 1 || p.findings[0].commentID != 11 || len(p.dismissed) != 1 {
		t.Fatalf("findings = %+v, dismissed = %+v; want the dismissed finding moved apart", p.findings, p.dismissed)
	}
	want := []review.DismissedFinding{{Finding: gone.Finding, Reason: "intended"}}
	if got := dismissedFindings(p.dismissed); !reflect.DeepEqual(got, want) {
		t.Fatalf("dismissedFindings = %+v, want %+v", got, want)
	}
}

func TestDropDismissed(t *testing.T) {
	a := review.Finding{Path: "a.go", Line: 3, Title: "Unchecked error"}
	b := review.Finding{Path: "b.go", Line: 9, Title: "stale COMMENT"}
	dismissed := []store.Dismissal{{Fingerprint: review.Fingerprint(review.Finding{Path: "b.go", Title: "Stale comment"})}}
	kept, n := dropDismissed([]review.Finding{a, b}, dismissed)
	if n != 1 || len(kept) != 1 || kept[0].Title != a.Title {
		t.Fatalf("dropDismissed = %+v, %d; want the dismissed finding left out", kept, n)
	}
	if kept, n := dropDismissed([]review.Finding{a, b}, nil); n != 0 || len(kept) != 2 {
		t.Fatalf("dropDismissed without dismissals = %+v, %d", kept, n)
	}
}
