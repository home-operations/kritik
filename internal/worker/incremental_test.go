package worker

import (
	"testing"

	"github.com/home-operations/kritik/internal/review"
)

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
