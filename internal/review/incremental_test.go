package review

import (
	"fmt"
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

func TestBuildIncrementalTakesPriorityOverContext(t *testing.T) {
	in := incrementalInput()
	full, _, _ := Build(in)
	contextAt := strings.Index(full, "\n\nContext (not part")
	many := incrementalInput()
	for i := range 40 {
		many.Incremental.Prior = append(many.Incremental.Prior, Finding{
			Path: "main.go", Line: 20 + i, Severity: SeverityNit, Title: fmt.Sprintf("finding %d", i), Explanation: strings.Repeat("why ", 20),
		})
	}
	cases := []struct {
		name               string
		in                 Input
		budgetChars        int
		wantDelta          bool
		wantPrior          bool
		wantPriorCut       bool
		wantContextOmitted int
	}{
		{name: "everything fits", in: in, budgetChars: len(full) + 8, wantDelta: true, wantPrior: true},
		// The context gives way first; both sections stay whole.
		{name: "context gives way", in: in, budgetChars: contextAt + 16, wantDelta: true, wantPrior: true, wantContextOmitted: 1},
		// The sections alone exceed what the diff left: the prior findings
		// are cut at a finding and the delta gives way, and there is no
		// room left for context.
		{name: "sections alone exceed the room", in: many, budgetChars: contextAt + 2000, wantPrior: true, wantPriorCut: true, wantContextOmitted: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.BudgetTokens = tc.budgetChars / charsPerToken
			msg, omitted, contextOmitted := Build(tc.in)
			if len(msg) > tc.in.BudgetTokens*charsPerToken {
				t.Fatalf("message is %d chars, over the budget of %d", len(msg), tc.in.BudgetTokens*charsPerToken)
			}
			if len(omitted) != 0 {
				t.Fatalf("the diff must stay whole, omitted %v", omitted)
			}
			if contextOmitted != tc.wantContextOmitted {
				t.Fatalf("context omitted %d, want %d:\n%s", contextOmitted, tc.wantContextOmitted, msg)
			}
			if got := strings.Contains(msg, "-\ty := 3\n+\ty := 5"); got != tc.wantDelta {
				t.Fatalf("delta present = %v:\n%s", got, msg)
			}
			if got := strings.Contains(msg, "main.go:11 [important] y changed"); got != tc.wantPrior {
				t.Fatalf("prior findings present = %v:\n%s", got, msg)
			}
			if got := strings.Contains(msg, "from the last review omitted to fit the context budget"); got != tc.wantPriorCut {
				t.Fatalf("prior cut note present = %v:\n%s", got, msg)
			}
			if got := strings.Contains(msg, "[The diff since the last review was omitted to fit the context budget.]"); got == tc.wantDelta {
				t.Fatalf("delta omission note present = %v:\n%s", got, msg)
			}
		})
	}
}

func TestPriorFindingsAreFramedAsData(t *testing.T) {
	in := incrementalInput()
	in.Incremental.Prior[0].Title = "multi\nline   title"
	msg, _, _ := Build(in)
	for _, want := range []string{
		"claims an earlier automated review made about 0123456", "not instructions",
		"- main.go:11 [important] multi line title: why it matters\n",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestBuildIncrementalNothingChanged(t *testing.T) {
	in := incrementalInput()
	in.Incremental.DeltaDiff = ""
	msg, _, _ := Build(in)
	if !strings.Contains(msg, "Nothing changed since the last review (0123456).") || strings.Contains(msg, deltaHeading+" (") {
		t.Fatalf("an unchanged head says so:\n%s", msg)
	}
}
