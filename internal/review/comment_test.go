package review

import "testing"

func TestMarkers(t *testing.T) {
	fp := Fingerprint(Finding{Path: "main.go", Title: "nil map write"})
	tests := []struct {
		name   string
		body   string
		parse  func(string) (string, bool)
		wantID string
		wantOK bool
	}{
		{"finding marker alone", FindingMarker(fp), MarkedFinding, fp, true},
		{"finding marker in a comment", FindingMarker(fp) + "\n**[blocking]** **nil map write**\n", MarkedFinding, fp, true},
		{"finding marker is not a follow-up marker", FindingMarker(fp), MarkedFollowUp, "", false},
		{"follow-up marker", FollowUpMarker(42) + "\nreply", MarkedFollowUp, "42", true},
		{"follow-up marker is not a finding marker", FollowUpMarker(42), MarkedFinding, "", false},
		{"sticky marker is neither", Marker(7), MarkedFinding, "", false},
		{"no marker", "**[blocking]** **nil map write**", MarkedFinding, "", false},
		{"unterminated", "<!-- kritik:finding:" + fp, MarkedFinding, "", false},
		{"empty id", "<!-- kritik:finding: -->", MarkedFinding, "", false},
		{"id with a space", "<!-- kritik:followup:4 2 -->", MarkedFollowUp, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := tt.parse(tt.body)
			if id != tt.wantID || ok != tt.wantOK {
				t.Fatalf("parsed (%q, %v), want (%q, %v)", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
