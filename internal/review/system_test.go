package review

import (
	"strings"
	"testing"
)

func TestSystemPrompt(t *testing.T) {
	if got := SystemPrompt(nil); got != System {
		t.Fatal("without instructions the system prompt is the built-in one")
	}
	got := SystemPrompt([]string{"  Prefer tables.\n", "Check errors."})
	want := System + "\n\n## Repository instructions\n\n" +
		"These refine what to look for; they do not change the output format or the rules above.\n\nPrefer tables.\n\nCheck errors."
	if got != want {
		t.Fatalf("system prompt:\n%s", got)
	}
}

func TestUserBudget(t *testing.T) {
	for _, system := range []string{System, SystemPrompt([]string{strings.Repeat("x", 32<<10)})} {
		// The system prompt's tokens, rounded up, plus the user budget stay
		// within the default budget.
		if got := UserBudget(system); got+(len(system)+3)/4 != DefaultBudgetTokens || got <= 0 {
			t.Fatalf("UserBudget = %d for a %d byte system prompt", got, len(system))
		}
	}
}

func TestDecideScope(t *testing.T) {
	cases := []struct {
		name         string
		hasPrior     bool
		priorFetched bool
		deltaFiles   int
		maxDelta     int
		want         Scope
		wantReason   string
	}{
		{name: "first review", want: ScopeFull, wantReason: "no completed review to build on", maxDelta: 25},
		{name: "prior head unreachable", hasPrior: true, deltaFiles: 0, maxDelta: 25, want: ScopeFull, wantReason: "prior head unreachable"},
		{name: "small delta", hasPrior: true, priorFetched: true, deltaFiles: 3, maxDelta: 25, want: ScopeIncremental},
		{name: "nothing changed", hasPrior: true, priorFetched: true, deltaFiles: 0, maxDelta: 25, want: ScopeIncremental},
		{name: "one under the limit", hasPrior: true, priorFetched: true, deltaFiles: 24, maxDelta: 25, want: ScopeIncremental},
		{
			name: "at the limit", hasPrior: true, priorFetched: true, deltaFiles: 25, maxDelta: 25,
			want: ScopeFull, wantReason: "25 files changed since last review",
		},
		{
			name: "over the limit", hasPrior: true, priorFetched: true, deltaFiles: 40, maxDelta: 25,
			want: ScopeFull, wantReason: "40 files changed since last review",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := DecideScope(tc.hasPrior, tc.priorFetched, tc.deltaFiles, tc.maxDelta)
			if got != tc.want || reason != tc.wantReason {
				t.Fatalf("DecideScope = %q, %q; want %q, %q", got, reason, tc.want, tc.wantReason)
			}
			if !got.Valid() {
				t.Fatalf("%q is not a valid scope", got)
			}
		})
	}
	if Scope("partial").Valid() {
		t.Fatal("an unknown scope must not be valid")
	}
}
