package configfile

import (
	"strings"
	"testing"
	"time"
)

const authMinimal = `auth:
  sessionTTL: 8h
  admin:
    password: { env: TEST_ADMIN_PASSWORD }
  oidc:
    name: Zitadel
    issuer: https://sso.example.com
    clientId: kritika
    clientSecret: { env: TEST_WEBHOOK_SECRET }
    scopes: [openid, profile, email]
    rolesClaim: "urn:zitadel:iam:org:project:1:roles"
    roleMappingExpr: '"kritika-admin" in roles ? "admin" : ("kritika-user" in roles ? "member" : "")'
    defaultRole: none
  github:
    clientId: Iv1.x
    clientSecret: { env: TEST_WEBHOOK_SECRET }
    roleMappingExpr: 'login == "user-1" ? "admin" : ""'
`

func authEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_PRIVATE_KEY", "key")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_ADMIN_PASSWORD", "hunter2")
}

func TestAuth(t *testing.T) {
	authEnv(t)
	f, err := loadBytes(t, []byte(authMinimal+minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := f.Auth
	if user, pw, ok := a.AdminUser(); !ok || user != DefaultAdminUser || pw.Value() != "hunter2" {
		t.Fatalf("AdminUser = %q, %v", user, ok)
	}
	oidc, ok := a.SignInByType(SignInOIDC)
	if !ok || oidc.Label() != "Zitadel" || oidc.ClientSecretValue().Value() != "whsec" || oidc.Mapping() == nil || oidc.MembersByDefault() {
		t.Fatalf("oidc = %+v, %v", oidc, ok)
	}
	gh, ok := a.SignInByType(SignInGitHub)
	if !ok || gh.Label() != "GitHub" || gh.Type() != SignInGitHub || gh.Mapping() == nil {
		t.Fatalf("github = %+v, %v", gh, ok)
	}
	if got := a.SignIns(); len(got) != 2 || got[0] != oidc || got[1] != gh {
		t.Fatalf("SignIns = %v", got)
	}
	if a.SessionTTLOrDefault() != 8*time.Hour || (&Auth{}).SessionTTLOrDefault() != DefaultSessionTTL || DefaultSessionTTL != 12*time.Hour {
		t.Fatal("session ttl")
	}
	if !a.Configured() || (&Auth{}).Configured() {
		t.Fatal("Configured")
	}
}

func TestAuthRejects(t *testing.T) {
	authEnv(t)
	t.Setenv("TEST_EMPTY", "")
	rep := func(o, n string) string { return strings.Replace(authMinimal, o, n, 1) + minimal }
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"oidc http issuer", rep("issuer: https://", "issuer: http://"), "auth.oidc.issuer"},
		{"oidc without issuer", rep("    issuer: https://sso.example.com\n", ""), "auth.oidc.issuer"},
		{"issuer on github", rep("    clientId: Iv1.x\n", "    clientId: Iv1.x\n    issuer: https://x.example.com\n"), "auth.github.issuer is for oidc"},
		{"scopes on github", rep("    clientId: Iv1.x\n", "    clientId: Iv1.x\n    scopes: [read:user]\n"), "auth.github.scopes is for oidc"},
		{"github enterprise host", rep("    clientId: Iv1.x\n", "    clientId: Iv1.x\n    host: github.example.com\n"), "field host not found"},
		{"no client id", rep("clientId: kritika", "clientId: \"\""), "auth.oidc.clientId is required"},
		{"unset secret", rep("{ env: TEST_WEBHOOK_SECRET }", "{ env: TEST_NOPE }"), "auth.oidc.clientSecret"},
		{"empty secret", rep("{ env: TEST_WEBHOOK_SECRET }", "{ env: TEST_EMPTY }"), "auth.oidc.clientSecret resolved to an empty value"},
		{"sealed secret", rep("{ env: TEST_WEBHOOK_SECRET }", "{ sealed: abc }"), "field sealed not found"},
		{"empty password", rep("TEST_ADMIN_PASSWORD", "TEST_EMPTY"), "auth.admin.password resolved to an empty value"},
		{"blank admin user", rep("  admin:\n", "  admin:\n    user: ' '\n"), "auth.admin.user must not be blank"},
		{"unknown default role", rep("defaultRole: none", "defaultRole: admin"), "auth.oidc.defaultRole must be none or member"},
		{"bad mapping", rep(`roleMappingExpr: 'login == "user-1" ? "admin" : ""'`, `roleMappingExpr: 'claims.x'`), "auth.github.roleMappingExpr"},
		{"ttl too short", rep("sessionTTL: 8h", "sessionTTL: 1m"), "auth.sessionTTL"},
		{"ttl too long", rep("sessionTTL: 8h", "sessionTTL: 800h"), "auth.sessionTTL"},
		{"the old web block", "web:\n  sessionTTL: 8h\n" + minimal, "field web not found"},
		{"no way to an admin", `auth:
  github:
    clientId: Iv1.x
    clientSecret: { env: TEST_WEBHOOK_SECRET }
` + minimal, "no way to sign in as an admin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadBytes(t, []byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not mention %q", err, tt.want)
			}
		})
	}
}

// TestAuthEnv: every KRITIKA_AUTH_* variable sets its key over the file's,
// a secret's variable carries the value, and a variable naming no key is
// refused.
func TestAuthEnv(t *testing.T) {
	authEnv(t)
	for k, v := range map[string]string{
		"KRITIKA_AUTH_SESSION_TTL":              "1h",
		"KRITIKA_AUTH_ADMIN_USER":               "root",
		"KRITIKA_AUTH_ADMIN_PASSWORD":           "from-env",
		"KRITIKA_AUTH_OIDC_NAME":                "SSO",
		"KRITIKA_AUTH_OIDC_ISSUER":              "https://idp.example.com",
		"KRITIKA_AUTH_OIDC_SCOPES":              "openid, groups ,",
		"KRITIKA_AUTH_OIDC_DEFAULT_ROLE":        "member",
		"KRITIKA_AUTH_GITHUB_CLIENT_ID":         "Iv1.env",
		"KRITIKA_AUTH_GITHUB_CLIENT_SECRET":     "gh-secret\n",
		"KRITIKA_AUTH_GITHUB_ROLE_MAPPING_EXPR": `"org-1" in orgs ? "admin" : ""`,
	} {
		t.Setenv(k, v)
	}
	f, err := loadBytes(t, []byte(authMinimal+minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := f.Auth
	oidc, _ := a.SignInByType(SignInOIDC)
	gh, _ := a.SignInByType(SignInGitHub)
	user, pw, _ := a.AdminUser()
	switch {
	case a.SessionTTL != time.Hour, user != "root", pw.Value() != "from-env":
		t.Fatalf("session or admin = %s %q", a.SessionTTL, user)
	case oidc.Label() != "SSO", oidc.Issuer != "https://idp.example.com", strings.Join(oidc.Scopes, " ") != "openid groups",
		!oidc.MembersByDefault(), oidc.ClientID != "kritika":
		t.Fatalf("oidc = %+v", oidc)
	case gh.ClientID != "Iv1.env", gh.ClientSecretValue().Value() != "gh-secret", gh.RoleMappingExpr != `"org-1" in orgs ? "admin" : ""`:
		t.Fatalf("github = %+v", gh)
	}
	for path, want := range map[string]bool{"oidc.issuer": true, "github.clientSecret": true, "oidc.clientId": false} {
		if a.FromEnv(path) != want {
			t.Errorf("FromEnv(%s) = %v", path, !want)
		}
	}

	t.Run("without a file block", func(t *testing.T) {
		t.Setenv("KRITIKA_AUTH_OIDC_CLIENT_ID", "kritika")
		t.Setenv("KRITIKA_AUTH_OIDC_CLIENT_SECRET", "oidc-secret")
		f, err := loadBytes(t, []byte(minimal))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if _, ok := f.Auth.SignInByType(SignInGitHub); !ok {
			t.Fatal("the environment alone did not configure github")
		}
	})
	t.Run("a secret from a file", func(t *testing.T) {
		t.Setenv("KRITIKA_AUTH_GITHUB_CLIENT_SECRET_FILE", "/var/run/secrets/gh-secret")
		if _, err := loadBytes(t, []byte(minimal)); err == nil ||
			!strings.Contains(err.Error(), "KRITIKA_AUTH_GITHUB_CLIENT_SECRET_FILE names no auth setting") {
			t.Fatalf("Parse = %v", err)
		}
	})
	t.Run("a variable naming nothing", func(t *testing.T) {
		t.Setenv("KRITIKA_AUTH_OIDC_ISUER", "x")
		if _, err := loadBytes(t, []byte(minimal)); err == nil || !strings.Contains(err.Error(), "KRITIKA_AUTH_OIDC_ISUER names no auth setting") {
			t.Fatalf("Parse = %v", err)
		}
	})
}
