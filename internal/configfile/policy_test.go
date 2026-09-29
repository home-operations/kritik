package configfile

import (
	"slices"
	"testing"
)

// TestPoliciesNameSettings checks every row of the table names a setting
// each scope it lists has, and no scope it leaves out.
func TestPoliciesNameSettings(t *testing.T) {
	specs := map[Scope]any{ScopeDefaults: &Defaults{}, ScopeAccount: &Account{}, ScopeRepository: &Repository{}}
	for _, p := range Policies {
		for scope, spec := range specs {
			_, ok := SpecValue(spec, p.Key)
			if want := slices.Contains(p.Scopes, scope); ok != want {
				t.Errorf("%s at %s: found %v, want %v", p.Key, scope, ok, want)
			}
		}
	}
}

func TestSpecValue(t *testing.T) {
	steps := 5
	r := &Repository{Name: "a/b", Agent: Agent{MaxSteps: &steps}, Mode: ReviewAgentic}
	if v, ok := SpecValue(r, "agent.maxSteps"); !ok || *v.(*int) != 5 {
		t.Fatalf("agent.maxSteps = %v, %v", v, ok)
	}
	if v, ok := SpecValue(r, "mode"); !ok || v.(ReviewMode) != ReviewAgentic {
		t.Fatalf("mode = %v, %v", v, ok)
	}
	if v, ok := SpecValue(&Account{}, "runner"); !ok || v.(*Runner) != nil {
		t.Fatalf("runner = %v, %v", v, ok)
	}
	if v, ok := SpecValue(&Account{}, "limits.concurrency"); !ok || v.(*int) != nil {
		t.Fatalf("limits.concurrency = %v, %v", v, ok)
	}
	if v, ok := SpecValue(&Account{}, "enabled"); !ok || v.(*bool) != nil {
		t.Fatalf("enabled = %v, %v", v, ok)
	}
}

func TestSources(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f := mustLoad(t, "defaults:\n  settle: 2m\n  agent: { maxSteps: 9 }\n"+
		acme("    mode: agentic\n    repositories: [{ name: x, settle: 0s, enabled: false }]\n"))
	s := f.Sources(&f.Accounts[0], "acme/x")
	for key, want := range map[string]Source{
		"settle": SourceAccount, "mode": SourceAccount, "agent.maxSteps": SourceDefaults, "enabled": SourceAccount,
		"agent.maxTokens": SourceDefault, "models.review": SourceDefault, "ignore": SourceDefault,
	} {
		if s[key] != want {
			t.Errorf("%s from %s, want %s", key, s[key], want)
		}
	}
	if _, ok := s["skip.onlyPaths"]; ok {
		t.Error("a setting only the repository has has no admin source")
	}
	if s := f.Sources(&Account{Forge: ForgeGitHub, Name: "other"}, "other/x"); s["settle"] != SourceDefaults || s["mode"] != SourceDefault {
		t.Fatalf("an account without an entry = %v", s)
	}
}
