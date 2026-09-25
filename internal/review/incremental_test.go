package review

import (
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/contextpack"
)

const deltaDiff = `diff --git a/main.go b/main.go
index 222..555 100644
--- a/main.go
+++ b/main.go
@@ -11,1 +11,1 @@
-	y := 3
+	y := 5
`

const (
	deltaHeading = "Changed since the last review"
	priorHeading = "Findings from the last review (verify each; report again only if still present)"
)

func incrementalInput() Input {
	return Input{
		Repository: "acme/widgets", Number: 1, Title: "t", Author: "u", BaseRef: "main", Changed: []string{"main.go", "README.md"},
		Diff: sampleDiff,
		Context: []contextpack.Chunk{{
			Stage: "overlay", Path: "main.go", Language: "go", Symbol: "a", Kind: "function", StartLine: 9, EndLine: 15,
			Text: "func a() {\n" + strings.Repeat("\t// filler\n", 150) + "}",
		}},
		Incremental: &IncrementalInput{
			PriorHeadSHA: "0123456789abcdef0123456789abcdef01234567",
			DeltaDiff:    deltaDiff,
			Prior: []Finding{
				{Path: "main.go", Line: 11, Severity: SeverityImportant, Title: "y changed", Explanation: "why\nit matters"},
			},
		},
	}
}

func TestBuildIncrementalRendersBothSections(t *testing.T) {
	msg, omitted, contextOmitted := Build(incrementalInput())
	if len(omitted) != 0 || contextOmitted != 0 {
		t.Fatalf("omitted %v, context omitted %d", omitted, contextOmitted)
	}
	for _, want := range []string{
		deltaHeading + " (0123456", "-\ty := 3\n+\ty := 5", priorHeading, "- main.go:11 [important] y changed: why it matters",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
	diffAt, deltaAt, priorAt, contextAt := strings.Index(msg, "Diff (unified"), strings.Index(msg, deltaHeading),
		strings.Index(msg, priorHeading), strings.Index(msg, "Context (not part")
	if diffAt >= deltaAt || deltaAt >= priorAt || priorAt >= contextAt {
		t.Fatalf("want diff, delta, prior findings, context in that order:\n%s", msg)
	}

	plain := incrementalInput()
	plain.Incremental = nil
	if msg, _, _ := Build(plain); strings.Contains(msg, deltaHeading) || strings.Contains(msg, priorHeading) {
		t.Fatalf("a full review has no incremental sections:\n%s", msg)
	}
}

func TestBuildIncrementalIsCutBeforeContext(t *testing.T) {
	in := incrementalInput()
	full, _, _ := Build(in)
	cases := []struct {
		name                 string
		budgetChars          int
		wantDelta, wantPrior bool
		wantContextOmitted   int
		wantDeltaOmittedNote bool
	}{
		{name: "everything fits", budgetChars: len(full) + 8, wantDelta: true, wantPrior: true},
		// Only the incremental sections are short of room: they give way
		// and the context stays whole.
		{name: "incremental sections give way", budgetChars: len(full) - len(deltaDiff), wantPrior: true, wantDeltaOmittedNote: true},
		{name: "context alone fits", budgetChars: len(full) - len(deltaDiff) - 200, wantDeltaOmittedNote: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in.BudgetTokens = tc.budgetChars / charsPerToken
			msg, _, contextOmitted := Build(in)
			if len(msg) > in.BudgetTokens*charsPerToken {
				t.Fatalf("message is %d chars, over the budget of %d", len(msg), in.BudgetTokens*charsPerToken)
			}
			if contextOmitted != tc.wantContextOmitted || !strings.Contains(msg, "### overlay: main.go") {
				t.Fatalf("context omitted %d:\n%s", contextOmitted, msg)
			}
			if got := strings.Contains(msg, "-\ty := 3\n+\ty := 5"); got != tc.wantDelta {
				t.Fatalf("delta present = %v:\n%s", got, msg)
			}
			if got := strings.Contains(msg, "main.go:11 [important] y changed"); got != tc.wantPrior {
				t.Fatalf("prior findings present = %v:\n%s", got, msg)
			}
			if got := strings.Contains(msg, "since the last review was omitted to fit the context budget"); got != tc.wantDeltaOmittedNote {
				t.Fatalf("delta omission note present = %v:\n%s", got, msg)
			}
		})
	}
}
