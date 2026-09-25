package configfile

import (
	"slices"
	"strings"
	"testing"
)

// minimalEnv sets what the minimal fixture's secret references resolve.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_FORGEJO_TOKEN", "fj-token")
	t.Setenv("TEST_CLIENT_ID", "Iv1.fromenv")
}

func TestEgressRules(t *testing.T) {
	t.Setenv("TEST_GH_TOKEN", "ghp_x\n")
	f, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture names a GitHub installation, whose forge must be allowed
	// without being listed, and an OpenRouter provider, which runners reach
	// only through the gateway's model endpoint.
	rules := f.EgressRules()
	if !rules.Allows("github.com") {
		t.Errorf("implicit host github.com not allowed; hosts = %v", rules.Hosts)
	}
	if rules.Allows("openrouter.ai") {
		t.Errorf("a provider's host must not be allowed; hosts = %v", rules.Hosts)
	}
	if rules.Allows("ghcr.io") {
		t.Fatal("ghcr.io must not be allowed until configured")
	}

	raw := "egress:\n  allowHosts: [ghcr.io, \"*.githubusercontent.com\", api.github.com]\n" +
		"  credentials:\n    api.github.com: { env: TEST_GH_TOKEN }\n"
	g, err := Parse([]byte(raw + minimal))
	if err != nil {
		t.Fatal(err)
	}
	rules = g.EgressRules()
	for host, want := range map[string]bool{"ghcr.io": true, "raw.githubusercontent.com": true, "api.github.com": true, "evil.example": false} {
		if rules.Allows(host) != want {
			t.Errorf("Allows(%s) = %v, want %v", host, !want, want)
		}
	}
	if rules.Credentials["api.github.com"] != "Bearer ghp_x" {
		t.Fatalf("credentials = %v", rules.Credentials)
	}
	// A Forgejo installation's host is allowed implicitly, a provider's
	// baseUrl is not, and a credential's value never leaks into the host
	// list.
	h, err := Parse([]byte("providers:\n  p:\n    type: openai\n    baseUrl: https://llm.example:8443/v1\n    apiKey: { env: TEST_GH_TOKEN }\n" +
		strings.Replace(minimal, "account: acme", "host: git.example.org\n        account: acme", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if hosts := h.EgressRules().Hosts; !slices.Contains(hosts, "git.example.org") || slices.Contains(hosts, "llm.example") ||
		slices.Contains(hosts, "api.openai.com") || slices.ContainsFunc(hosts, func(h string) bool { return strings.Contains(h, "ghp_") }) {
		t.Fatalf("hosts = %v", hosts)
	}
}

func TestEgressRejects(t *testing.T) {
	minimalEnv(t)
	t.Setenv("TEST_GH_TOKEN", "ghp_x")
	t.Setenv("TEST_EMPTY", "")
	for name, tt := range map[string]struct{ yaml, want string }{
		"scheme in host":         {"egress:\n  allowHosts: [\"https://ghcr.io\"]\n", "lowercase hostname"},
		"port in host":           {"egress:\n  allowHosts: [\"ghcr.io:443\"]\n", "lowercase hostname"},
		"uppercase host":         {"egress:\n  allowHosts: [GHCR.io]\n", "lowercase hostname"},
		"credential not allowed": {"egress:\n  credentials:\n    api.github.com: { env: TEST_GH_TOKEN }\n", "not in egress.allowHosts"},
		"empty credential":       {"egress:\n  allowHosts: [api.github.com]\n  credentials:\n    api.github.com: { env: TEST_EMPTY }\n", "empty value"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml + minimal))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
