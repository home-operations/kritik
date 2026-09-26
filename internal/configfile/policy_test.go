package configfile

import (
	"slices"
	"strings"
	"testing"
)

// TestPoliciesNameSettings checks every row of the table names a setting
// each scope it lists has, and no scope it leaves out.
func TestPoliciesNameSettings(t *testing.T) {
	specs := map[Scope]any{ScopeDefaults: &Defaults{}, ScopeTenant: &Tenant{}, ScopeRepository: &Repository{}}
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
	if v, ok := SpecValue(&Tenant{}, "runner"); !ok || v.(*Runner) != nil {
		t.Fatalf("runner = %v, %v", v, ok)
	}
	if v, ok := SpecValue(&Tenant{}, "limits.concurrency"); !ok || v.(*int) != nil {
		t.Fatalf("limits.concurrency = %v, %v", v, ok)
	}
	if _, ok := SpecValue(&Tenant{}, "enabled"); ok {
		t.Fatal("a tenant has no enabled key")
	}
}

func TestSources(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f, err := Parse([]byte("defaults:\n  settle: 2m\n  agent: { maxSteps: 9 }\n" + strings.Replace(minimal, "slug: acme",
		"slug: acme\n    mode: agentic\n    repositories: [{ name: acme/x, settle: 0s, enabled: false }]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	s := f.Sources(&f.Tenants[0], "acme-bot", "acme/x")
	for key, want := range map[string]Source{
		"settle": SourceFile, "mode": SourceFile, "agent.maxSteps": SourceFile, "enabled": SourceFile,
		"agent.maxTokens": SourceDefault, "models.review": SourceDefault, "ignore": SourceDefault,
	} {
		if s[key] != want {
			t.Errorf("%s from %s, want %s", key, s[key], want)
		}
	}
	if _, ok := s["skip.onlyPaths"]; ok {
		t.Error("a setting only the repository has has no operator source")
	}
	dash, err := DecodeTenant(DashboardTenant{Slug: "beta", Spec: []byte(`{"slug":"beta","filter":"true","installations":[{"name":"b"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if s := f.Sources(&dash, "b", "beta/x"); s["filter"] != SourceDashboard || s["settle"] != SourceFile {
		t.Fatalf("dashboard tenant sources = %v", s)
	}
}
