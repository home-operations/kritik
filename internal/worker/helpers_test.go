package worker

import (
	"errors"
	"testing"

	"github.com/home-operations/kritik/internal/executor"
)

func TestSmallHelpers(t *testing.T) {
	if errText(nil) != "" || errText(errors.New("x")) != "x" {
		t.Fatal("errText")
	}
	if callOutcome(nil) != "ok" || callOutcome(errors.New("x")) != "error" {
		t.Fatal("callOutcome")
	}
	if embedText(stagedChunk{path: "a/b.go", symbol: "Build", kind: "function", text: "func Build() {}"}) != "a/b.go function Build\nfunc Build() {}" {
		t.Fatal("embedText with a symbol")
	}
	if embedText(stagedChunk{path: "values.yaml", text: "a: 1"}) != "values.yaml\na: 1" {
		t.Fatal("embedText without a symbol")
	}
}

func TestRunOutcomeAndMention(t *testing.T) {
	cases := []struct {
		res  executor.Result
		want string
	}{
		{executor.Result{}, "success"},
		{executor.Result{Err: errors.New("x")}, "failed"},
		{executor.Result{Err: errors.New("x"), DeadlineExceeded: true}, "deadline"},
	}
	for _, c := range cases {
		if got := runOutcome(c.res); got != c.want {
			t.Errorf("runOutcome(%+v) = %s, want %s", c.res, got, c.want)
		}
	}
	mentions := map[string]bool{
		"@kritik please":     true,
		"hey @Kritik, why?":  true,
		"email me@kritik.io": false,
		"@kritikbot no":      false,
		"no mention":         false,
	}
	for body, want := range mentions {
		if got := mentioned(body, "kritik"); got != want {
			t.Errorf("mentioned(%q) = %v, want %v", body, got, want)
		}
	}
}
