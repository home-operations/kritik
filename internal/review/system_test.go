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

func TestAgenticSystemPrompt(t *testing.T) {
	got := AgenticSystemPrompt([]string{"Check errors."})
	if !strings.HasPrefix(got, "You are kritik") || strings.Contains(got, "You see the diff of the change and nothing else") {
		t.Fatalf("the agentic prompt must not claim the diff is all it sees:\n%s", got)
	}
	for _, want := range []string{"read_file", "grep", "list_files", "verify", "only to lines the diff shows",
		"call submit_review exactly once"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// The shared rules and the instructions come through unchanged, with
	// the instructions last.
	rules := System[strings.Index(System, "Report only things"):]
	if !strings.Contains(got, rules) || !strings.HasSuffix(got, "\n\nCheck errors.") {
		t.Fatalf("agentic prompt:\n%s", got)
	}
	if !strings.Contains(System, "You see the diff of the change and nothing else") {
		t.Fatal("the single-mode prompt changed")
	}
}

// TestSystemRulesInBothModes pins the rules that shape what is reported,
// which both the single-shot and the agentic reviewer must share.
func TestSystemRulesInBothModes(t *testing.T) {
	for _, want := range []string{
		"ask whether a maintainer would stop the review for it",
		"Never\nreport: comments or docstrings to add",
		"you do not recognise is not a finding",
		"A finding you would have to hedge (may, could, appears to)",
		"mentions a concern only if it is also a finding",
		"It does not say what the diff cannot show",
	} {
		for name, system := range map[string]string{"single": System, "agentic": AgenticSystemPrompt(nil)} {
			if !strings.Contains(system, want) {
				t.Fatalf("%s prompt lacks %q", name, want)
			}
		}
	}
}
