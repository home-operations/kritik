package review

import (
	"strings"
	"testing"
)

func TestBuildDismissed(t *testing.T) {
	in := Input{
		Repository: "acme/widgets", Diff: "diff --git a/a.go b/a.go\n+x\n",
		Dismissed: []DismissedFinding{
			{Path: "a.go", Line: 3, Severity: SeverityImportant, Title: "Unchecked error", Explanation: "err is dropped.", Reason: "the caller\nchecks it"},
			{Path: "b.go", Line: 9, Severity: SeverityNit, Title: "Naming", Explanation: "x is terse."},
		},
		References: []Reference{{Path: "ARCH.md", Description: "the shape"}},
	}
	for i := range 8 {
		in.Dismissed = append(in.Dismissed, DismissedFinding{Path: "c.go", Line: i, Severity: SeverityNit, Title: "Padding",
			Explanation: strings.Repeat("words ", 30)})
	}
	msg, _, _ := Build(in)
	for _, want := range []string{
		"Findings a maintainer dismissed on this pull request.",
		"- a.go:3 [important] Unchecked error: err is dropped. (dismissed: the caller checks it)\n",
		"- b.go:9 [nit] Naming: x is terse.\n",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
	if strings.Index(msg, "dismissed on this pull request") > strings.Index(msg, "Reference files") {
		t.Fatalf("the dismissed findings must precede the references:\n%s", msg)
	}
	in.BudgetTokens = (strings.Index(msg, "\n\nReference files") + 20) / charsPerToken
	if cut, _, _ := Build(in); strings.Contains(cut, "Reference files") || !strings.Contains(cut, "Unchecked error") || !strings.Contains(cut, "+x") {
		t.Fatalf("over budget, the references give way before the dismissed findings:\n%s", cut)
	}
}
