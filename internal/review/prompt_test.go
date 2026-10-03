package review

import (
	"strings"
	"testing"
)

func TestShortSHA(t *testing.T) {
	if ShortSHA("0123456789") != "0123456" || ShortSHA("abc") != "abc" {
		t.Fatal("ShortSHA")
	}
}

func TestFitDiffLeavesOutWhatDoesNotFit(t *testing.T) {
	small := "diff --git a/a.go b/a.go\n+a\n"
	large := "diff --git a/gen.lock b/gen.lock\n" + strings.Repeat("+x\n", 100)
	last := "diff --git a/z.go b/z.go\n+z\n"
	diff := small + large + last
	if got, omitted := FitDiff(diff, len(diff)); got != diff || omitted != nil {
		t.Fatalf("a diff that fits was cut: %v omitted", omitted)
	}
	got, omitted := FitDiff(diff, len(small)+len(last)+8)
	if !strings.Contains(got, "a/a.go") || !strings.Contains(got, "a/z.go") || strings.Contains(got, "gen.lock") {
		t.Fatalf("kept = %q, want the two small files", got)
	}
	if len(omitted) != 1 || omitted[0] != "gen.lock" {
		t.Fatalf("omitted = %v, want gen.lock", omitted)
	}
}
