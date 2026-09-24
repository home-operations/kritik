package review

import (
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/contextpack"
)

const sampleDiff = `diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -10,4 +10,6 @@ func a() {
 	x := 1
-	y := 2
+	y := 3
+	z := 4
 	return x + y
+}
diff --git a/README.md b/README.md
index 333..444 100644
--- a/README.md
+++ b/README.md
@@ -1,2 +1,3 @@
 # title
+new line
 tail
`

func TestAnchors(t *testing.T) {
	a := Anchors(sampleDiff)
	tests := []struct {
		path string
		line int
		want bool
	}{
		{"main.go", 10, true},  // context " x := 1"
		{"main.go", 11, true},  // "+ y := 3"
		{"main.go", 12, true},  // "+ z := 4"
		{"main.go", 13, true},  // context return
		{"main.go", 14, true},  // "+}"
		{"main.go", 15, false}, // past the hunk
		{"main.go", 9, false},  // before the hunk
		{"README.md", 2, true}, // "+new line"
		{"README.md", 3, true}, // context tail
		{"README.md", 4, false},
		{"other.go", 1, false},
	}
	for _, tt := range tests {
		if got := a[tt.path][tt.line]; got != tt.want {
			t.Errorf("%s:%d anchored = %v, want %v", tt.path, tt.line, got, tt.want)
		}
	}
}

func TestParseKeepsAnchoredFindingsAndDropsTheRest(t *testing.T) {
	raw := `{"summary": " Changes y and adds z. ", "findings": [
	  {"path": "main.go", "line": 11, "severity": "warning", "title": "y changed", "body": "why"},
	  {"path": "main.go", "line": 99, "severity": "error", "title": "off the diff", "body": "x"},
	  {"path": "nope.go", "line": 1, "severity": "info", "title": "unknown file", "body": "x"},
	  {"path": "README.md", "line": 2, "severity": "silly", "title": "bad severity becomes info", "body": "x"},
	  {"path": "main.go", "line": 10, "severity": "info", "title": "", "body": "no title"}
	]}`
	res, dropped, err := Parse(raw, Anchors(sampleDiff))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "Changes y and adds z." {
		t.Fatalf("summary = %q", res.Summary)
	}
	if len(res.Findings) != 2 || len(dropped) != 3 {
		t.Fatalf("kept %d dropped %d: %+v", len(res.Findings), len(dropped), res.Findings)
	}
	if res.Findings[0].Path != "README.md" || res.Findings[0].Severity != SeverityInfo || res.Findings[1].Line != 11 {
		t.Fatalf("findings = %+v", res.Findings)
	}
	if _, _, err := Parse("not json", nil); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestBuildFitsBudgetAtFileBoundaries(t *testing.T) {
	in := Input{Repository: "a/b", Number: 1, Title: "t", Author: "u", BaseRef: "main", Changed: []string{"main.go", "README.md"}, Diff: sampleDiff}
	full, omitted, _ := Build(in)
	if len(omitted) != 0 || !strings.Contains(full, "+new line") || !strings.Contains(full, "Pull request #1: t") {
		t.Fatalf("full build omitted %v:\n%s", omitted, full)
	}
	// A budget that fits the header and main.go but not README.md: the
	// header, the omission headroom, and the first file section.
	sections := splitFiles(sampleDiff)
	header := len(full) - len(sampleDiff)
	in.BudgetTokens = (header + 512 + len(sections[0].text) + 8) / charsPerToken
	msg, omitted, _ := Build(in)
	if len(omitted) != 1 || omitted[0] != "README.md" || strings.Contains(msg, "+new line") || !strings.Contains(msg, "omitted to fit") {
		t.Fatalf("omitted = %v\n%s", omitted, msg)
	}
	if !strings.Contains(msg, "+	z := 4") {
		t.Fatal("main.go should still be whole")
	}
}

func TestBuildAppendsContextWithinBudget(t *testing.T) {
	in := Input{Repository: "a/b", Number: 1, Diff: sampleDiff, Changed: []string{"main.go"}, Context: []contextpack.Chunk{
		// Long enough that a budget cut at the second chunk still leaves the
		// diff room, so the diff fit does not confound the context fit.
		{Stage: "overlay", Path: "main.go", Language: "go", Symbol: "a", Kind: "function", StartLine: 9, EndLine: 15, Text: "func a() {\n" + strings.Repeat("\t// filler\n", 150) + "}"},
		{Stage: "caller", Path: "b.go", Language: "go", Symbol: "b", Kind: "function", Scope: "T", Ref: "a", StartLine: 1, EndLine: 3, Text: "func (T) b() { a() }"},
	}}
	msg, _, contextOmitted := Build(in)
	if contextOmitted != 0 || !strings.Contains(msg, "### overlay: main.go lines 9-15 (function a)") ||
		!strings.Contains(msg, "### caller: b.go lines 1-3 (function b in T) for a") || !strings.Contains(msg, "```go\nfunc (T) b() { a() }\n```") {
		t.Fatalf("context omitted %d:\n%s", contextOmitted, msg)
	}
	if !strings.Contains(msg, "Context (not part of the diff") || strings.Index(msg, "Diff (unified") > strings.Index(msg, "Context (not part") {
		t.Fatal("context must follow the diff under its own heading")
	}
	// A budget that fits the diff and the first chunk only.
	in.BudgetTokens = (strings.Index(msg, "### caller") + 4) / charsPerToken
	msg, _, contextOmitted = Build(in)
	if contextOmitted != 1 || strings.Contains(msg, "### caller") || !strings.Contains(msg, "### overlay") {
		t.Fatalf("context omitted %d:\n%s", contextOmitted, msg)
	}
}

func TestBuildFollowUpAndParse(t *testing.T) {
	in := Input{Repository: "a/b", Number: 1, Title: "t", Author: "u", BaseRef: "main", Changed: []string{"main.go"}, Diff: sampleDiff}
	findings := []Finding{{Path: "main.go", Line: 11, Severity: SeverityWarning, Title: "y changed", Body: "why\nit matters"}}
	thread := []Message{
		{Author: "kritik[bot]", Body: "### kritik review\n\nFine."},
		{Author: "onedr0p", Body: "@kritik why is y changed?", When: time.Date(2026, 9, 24, 21, 0, 0, 0, time.UTC)},
	}
	msg := BuildFollowUp(in, findings, thread)
	for _, want := range []string{"Diff (unified", "+	z := 4", "Findings kritik posted on this pull request (1)", "main.go:11 [warning] y changed: why it matters",
		"--- kritik[bot] ---", "--- onedr0p (2026-09-24 21:00) [answer this] ---", "Reply to the last message from onedr0p."} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
	if strings.Index(msg, "Thread, oldest first") < strings.Index(msg, "Diff (unified") {
		t.Fatal("thread must come after the diff")
	}
	reply, err := ParseFollowUp(`{"reply": " Because the base value moved. "}`)
	if err != nil || reply != "Because the base value moved." {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	if _, err := ParseFollowUp(`{"reply": ""}`); err == nil {
		t.Fatal("an empty reply must error")
	}
	if !strings.HasPrefix(FollowUpBody(reply, "m"), reply) || !strings.Contains(FollowUpBody(reply, "m"), "kritik follow-up with m") {
		t.Fatal("FollowUpBody")
	}
}

func TestStickyBody(t *testing.T) {
	res := Result{Summary: "Fine.", Findings: []Finding{{Path: "main.go", Line: 11, Severity: SeverityError, Title: "boom", Body: "b"}}}
	body := StickyBody(42, res, "openai/gpt-6-sol", []string{"x"}, 1)
	for _, want := range []string{Marker(42), "### kritik review", "Fine.", "**1 finding(s)**", "`main.go:11` boom", "1 file(s) were omitted", "1 finding(s) could not be anchored", "openai/gpt-6-sol"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	empty := StickyBody(1, Result{Summary: "Nothing."}, "m", nil, 0)
	if !strings.Contains(empty, "Nothing worth flagging") || strings.Contains(empty, "omitted") {
		t.Fatalf("empty body:\n%s", empty)
	}
	if !strings.HasPrefix(InlineBody(res.Findings[0]), "🔴 **boom**") {
		t.Fatal("inline body should lead with the badge and title")
	}
}
