package worker

import (
	"testing"

	"github.com/home-operations/kritik/internal/review"
)

func TestDecideScope(t *testing.T) {
	cases := []struct {
		name         string
		hasPrior     bool
		priorFetched bool
		deltaFiles   int
		maxDelta     int
		want         reviewScope
		wantReason   string
	}{
		{name: "first review", want: scopeFull, wantReason: "no completed review to build on", maxDelta: 25},
		{name: "prior head unreachable", hasPrior: true, deltaFiles: 0, maxDelta: 25, want: scopeFull, wantReason: "prior head unreachable"},
		{name: "small delta", hasPrior: true, priorFetched: true, deltaFiles: 3, maxDelta: 25, want: scopeIncremental},
		{name: "nothing changed", hasPrior: true, priorFetched: true, deltaFiles: 0, maxDelta: 25, want: scopeIncremental},
		{name: "one under the limit", hasPrior: true, priorFetched: true, deltaFiles: 24, maxDelta: 25, want: scopeIncremental},
		{
			name: "at the limit", hasPrior: true, priorFetched: true, deltaFiles: 25, maxDelta: 25,
			want: scopeFull, wantReason: "25 files changed since last review",
		},
		{
			name: "over the limit", hasPrior: true, priorFetched: true, deltaFiles: 40, maxDelta: 25,
			want: scopeFull, wantReason: "40 files changed since last review",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := decideScope(tc.hasPrior, tc.priorFetched, tc.deltaFiles, tc.maxDelta)
			if got != tc.want || reason != tc.wantReason {
				t.Fatalf("decideScope = %q, %q; want %q, %q", got, reason, tc.want, tc.wantReason)
			}
			if !got.Valid() {
				t.Fatalf("%q is not a valid scope", got)
			}
		})
	}
	if reviewScope("partial").Valid() {
		t.Fatal("an unknown scope must not be valid")
	}
}

func TestAlreadyInline(t *testing.T) {
	seen := review.Finding{Path: "main.go", Line: 3, Title: "Nil  map write"}
	moved := review.Finding{Path: "main.go", Line: 9, Title: "nil map write"}
	fresh := review.Finding{Path: "main.go", Line: 5, Title: "unchecked error"}
	notPosted := review.Finding{Path: "util.go", Line: 1, Title: "slow loop"}
	prior := []priorFinding{
		{Finding: seen, postedInline: true},
		{Finding: notPosted, postedInline: false},
	}
	cases := []struct {
		name        string
		findings    []review.Finding
		prior       []priorFinding
		wantCarried []bool
	}{
		{name: "no prior review", findings: []review.Finding{seen, fresh}, wantCarried: []bool{false, false}},
		{
			// The fingerprint ignores the line and title case and spacing,
			// so a finding that moved is still the one already posted.
			name: "posted before", findings: []review.Finding{moved, fresh}, prior: prior, wantCarried: []bool{true, false},
		},
		{name: "found before but never posted", findings: []review.Finding{notPosted}, prior: prior, wantCarried: []bool{false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alreadyInline(tc.findings, tc.prior)
			if len(got) != len(tc.wantCarried) {
				t.Fatalf("alreadyInline = %v", got)
			}
			for i := range got {
				if got[i] != tc.wantCarried[i] {
					t.Fatalf("alreadyInline = %v, want %v", got, tc.wantCarried)
				}
			}
		})
	}
}
