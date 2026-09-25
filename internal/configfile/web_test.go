package configfile

import (
	"strings"
	"testing"
	"time"
)

const webMinimal = `web:
  signIn:
    - name: sso
      type: oidc
      issuer: https://sso.example.com/application/o/kritik/
      clientId: kritik
      clientSecret: { env: TEST_WEBHOOK_SECRET }
    - name: gh
      type: github
      clientId: Iv1.x
      clientSecret: { env: TEST_WEBHOOK_SECRET }
      scopes: [read:user]
    - name: fj
      type: forgejo
      host: git.example.com
      clientId: fj
      clientSecret: { env: TEST_WEBHOOK_SECRET }
  operators: ["sso:abc-123", "gh:octocat", "email:ops@example.com"]
  sessionTTL: 8h
`

func TestWeb(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f, err := Parse([]byte(webMinimal + minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sso, ok := f.Web.SignInByName("sso")
	if !ok || sso.Type != SignInOIDC || sso.ClientSecretValue().Value() != "whsec" {
		t.Fatalf("sso = %+v, %v", sso, ok)
	}
	gh, _ := f.Web.SignInByName("gh")
	if gh.Host != githubHost {
		t.Fatalf("github host = %q, want the default", gh.Host)
	}
	if _, ok := f.Web.SignInByName("nope"); ok {
		t.Fatal("found an undeclared sign-in")
	}
	if f.Web.SessionTTLOrDefault() != 8*time.Hour {
		t.Fatalf("session ttl = %s", f.Web.SessionTTLOrDefault())
	}
	if (Web{}).SessionTTLOrDefault() != DefaultSessionTTL || DefaultSessionTTL != 12*time.Hour {
		t.Fatal("session ttl default is not 12h")
	}

	m, err := Merge(f, nil, nil)
	if err != nil || len(m.Web.SignIn) != 3 {
		t.Fatalf("merge lost the web section: %v", err)
	}
}

func TestWebRejects(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_EMPTY", "")
	rep := func(o, n string) string { return strings.Replace(webMinimal, o, n, 1) + minimal }
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"bad name", rep("name: sso", "name: SSO"), "web.signIn[0].name"},
		{"reserved name", strings.ReplaceAll(rep("name: sso", "name: email"), `"sso:abc-123"`, `"email:a@b"`), "reserved"},
		{"duplicate name", rep("name: gh", "name: sso"), "duplicates web.signIn[0]"},
		{"bad type", rep("type: oidc", "type: saml"), "web.signIn[0].type must be"},
		{"oidc http issuer", rep("issuer: https://", "issuer: http://"), "https"},
		{"oidc without issuer", rep("      issuer: https://sso.example.com/application/o/kritik/\n", ""), "https"},
		{"issuer on github", rep("type: github\n", "type: github\n      issuer: https://x.example.com\n"), "web.signIn[1].issuer"},
		{"forgejo without host", rep("      host: git.example.com\n", ""), "web.signIn[2].host is required"},
		{"host on oidc", rep("type: oidc\n", "type: oidc\n      host: x.example.com\n"), "web.signIn[0].host"},
		{"no client id", rep("clientId: kritik", "clientId: \"\""), "web.signIn[0].clientId is required"},
		{"unset secret", rep("{ env: TEST_WEBHOOK_SECRET }", "{ env: TEST_NOPE }"), "web.signIn[0].clientSecret"},
		{"empty secret", rep("{ env: TEST_WEBHOOK_SECRET }", "{ env: TEST_EMPTY }"), "web.signIn[0].clientSecret resolved to an empty value"},
		{"sealed secret", rep("{ env: TEST_WEBHOOK_SECRET }", "{ sealed: abc }"), "sealed values are only valid"},
		{"operator of unknown sign-in", rep(`"gh:octocat"`, `"gl:octocat"`), "web.operators[1]"},
		{"operator without subject", rep(`"gh:octocat"`, `"gh:"`), "web.operators[1]"},
		{"operator without colon", rep(`"gh:octocat"`, `"octocat"`), "web.operators[1]"},
		{"operator email without address", rep(`"email:ops@example.com"`, `"email:"`), "web.operators[2]"},
		{"ttl too short", rep("sessionTTL: 8h", "sessionTTL: 1m"), "web.sessionTTL"},
		{"ttl too long", rep("sessionTTL: 8h", "sessionTTL: 800h"), "web.sessionTTL"},
		{"ttl negative", rep("sessionTTL: 8h", "sessionTTL: -1h"), "web.sessionTTL"},
		{"unknown key", rep("scopes:", "scope:"), "field scope not found"},
		{"dashboard forge host with scheme", rep("sessionTTL: 8h", "sessionTTL: 8h\n  dashboardForgeHosts: [https://git.example.com]"), "web.dashboardForgeHosts[0]"},
		{"dashboard forge host wildcard", rep("sessionTTL: 8h", "sessionTTL: 8h\n  dashboardForgeHosts: ['*.example.com']"), "web.dashboardForgeHosts[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not mention %q", err, tt.want)
			}
		})
	}
}

func TestRetentionTranscripts(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	tests := []struct {
		name string
		yaml string
		want time.Duration
		err  string
	}{
		{"default", "", 30 * 24 * time.Hour, ""},
		{"set", "retention:\n  transcripts: 48h\n", 48 * time.Hour, ""},
		{"too short", "retention:\n  transcripts: 1h\n", 0, "retention.transcripts"},
		{"negative", "retention:\n  transcripts: -48h\n", 0, "retention.transcripts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse([]byte(tt.yaml + minimal))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error %v does not mention %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Retention.TranscriptsOrDefault(); got != tt.want {
				t.Fatalf("transcripts = %s, want %s", got, tt.want)
			}
		})
	}
}
