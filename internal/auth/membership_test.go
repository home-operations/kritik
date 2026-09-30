package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
)

// connectionsYAML is the apps the grant tests read forge accounts from.
const connectionsYAML = `
apps:
  - { name: personal-bot, accounts: [Alice], clientId: Iv1.x, privateKey: { env: TEST_AUTH_SECRET }, webhookSecret: { env: TEST_AUTH_SECRET } }
  - { name: org-bot, accounts: [acme], clientId: Iv1.x, privateKey: { env: TEST_AUTH_SECRET }, webhookSecret: { env: TEST_AUTH_SECRET } }
  - { name: adminorg-bot, accounts: [widgets], clientId: Iv1.x, privateKey: { env: TEST_AUTH_SECRET }, webhookSecret: { env: TEST_AUTH_SECRET } }
  - { name: several-bot, accounts: [nobody, Initech], clientId: Iv1.x, privateKey: { env: TEST_AUTH_SECRET }, webhookSecret: { env: TEST_AUTH_SECRET } }
`

// adminPassword is an auth block's local admin, so a file whose sign-ins
// have no mapping still has a way to an admin.
const adminPassword = "  admin: { password: { env: TEST_AUTH_SECRET } }\n"

// testFile loads an auth block over connectionsYAML.
func testFile(t *testing.T, auth string) *configfile.File {
	t.Helper()
	t.Setenv("TEST_AUTH_SECRET", "s3cret")
	return configfiletest.Load(t, auth+connectionsYAML)
}

// orgs is a fake forge membership that remembers what it was asked.
type orgs struct {
	member map[string]bool
	err    error
	calls  []string
}

func (o *orgs) membership() Membership {
	return func(_ context.Context, org string) (bool, error) {
		o.calls = append(o.calls, org)
		if o.err != nil {
			return false, o.err
		}
		return o.member[strings.ToLower(org)], nil
	}
}

func githubFacts(o *orgs, login string, orgList ...string) Facts {
	return Facts{
		MappingVars: func(context.Context) (map[string]any, error) {
			return map[string]any{"login": login, "email": "", "orgs": orgList, "teams": []string{}}, nil
		},
		Membership: o.membership(),
	}
}

func oidcFacts(roles ...string) Facts {
	return Facts{MappingVars: func(context.Context) (map[string]any, error) {
		return map[string]any{"claims": map[string]any{"sub": "x"}, "roles": roles}, nil
	}}
}

func TestGrant(t *testing.T) {
	github := func(mapping string) string {
		auth := "auth:\n" + adminPassword + "  github:\n    clientId: Iv1.x\n    clientSecret: { env: TEST_AUTH_SECRET }\n"
		if mapping != "" {
			auth += "    roleMapping: '" + mapping + "'\n"
		}
		return auth
	}
	oidc := func(mapping, defaultRole string) string {
		auth := "auth:\n" + adminPassword + "  oidc:\n    issuer: https://id.example.com\n    clientId: k\n    clientSecret: { env: TEST_AUTH_SECRET }\n"
		if mapping != "" {
			auth += "    roleMapping: '" + mapping + "'\n"
		}
		if defaultRole != "" {
			auth += "    defaultRole: " + defaultRole + "\n"
		}
		return auth
	}
	tests := []struct {
		name     string
		auth     string
		typ      configfile.SignInType
		login    string
		facts    func(o *orgs) Facts
		member   map[string]bool
		role     Role
		all      bool
		accounts []string
		asked    bool
	}{
		{
			name: "the forge alone: own login and organizations", auth: github(""), typ: configfile.SignInGitHub, login: "alice",
			facts: func(o *orgs) Facts { return githubFacts(o, "alice") }, member: map[string]bool{"acme": true, "widgets": true},
			role: RoleMember, accounts: []string{"github/acme", "github/alice", "github/widgets"}, asked: true,
		},
		{
			name: "a mapping makes an admin without asking the forge", auth: github(`login == "alice" ? "admin" : ""`), typ: configfile.SignInGitHub,
			login: "alice", facts: func(o *orgs) Facts { return githubFacts(o, "alice") }, role: RoleAdmin,
		},
		{
			name: "a mapped account joins the forge's", auth: github(`{"github/Org-2": "member"}`), typ: configfile.SignInGitHub, login: "bob",
			facts: func(o *orgs) Facts { return githubFacts(o, "bob") }, member: map[string]bool{"acme": true},
			role: RoleMember, accounts: []string{"github/acme", "github/org-2"}, asked: true,
		},
		{
			name: "a mapped organization", auth: github(`"acme" in orgs ? "member" : ""`), typ: configfile.SignInGitHub, login: "bob",
			facts: func(o *orgs) Facts { return githubFacts(o, "bob", "acme") }, role: RoleMember, all: true,
		},
		{
			name: "an oidc member reads everything", auth: oidc(`"kritik-user" in roles ? "member" : ""`, ""), typ: configfile.SignInOIDC,
			facts: func(*orgs) Facts { return oidcFacts("kritik-user") }, role: RoleMember, all: true,
		},
		{
			name: "an oidc map of every account", auth: oidc(`{"*": "member"}`, ""), typ: configfile.SignInOIDC,
			facts: func(*orgs) Facts { return oidcFacts() }, role: RoleMember, all: true,
		},
		{
			name: "an oidc default of member", auth: oidc(`"kritik-admin" in roles ? "admin" : ""`, "member"), typ: configfile.SignInOIDC,
			facts: func(*orgs) Facts { return oidcFacts("other") }, role: RoleMember, all: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := testFile(t, tt.auth)
			s, _ := file.Auth.SignInByType(tt.typ)
			o := &orgs{member: tt.member}
			g, err := grant(context.Background(), file, s, Identity{Login: tt.login}, tt.facts(o))
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			if g.Role != tt.role || g.AllAccounts != tt.all || !slices.Equal(g.Accounts, tt.accounts) {
				t.Fatalf("grant = %+v, want role %s, all %v, accounts %v", g, tt.role, tt.all, tt.accounts)
			}
			if want, _ := GrantKey(file.Auth, string(tt.typ)); g.Key == "" || g.Key != want {
				t.Fatalf("key = %q, want %q", g.Key, want)
			}
			if (len(o.calls) > 0) != tt.asked {
				t.Fatalf("asked the forge about %v, want asked %v", o.calls, tt.asked)
			}
			seen := map[string]bool{}
			for _, c := range o.calls {
				if seen[strings.ToLower(c)] {
					t.Fatalf("org %s checked twice: %v", c, o.calls)
				}
				seen[strings.ToLower(c)] = true
			}
		})
	}
}

func TestGrantRefuses(t *testing.T) {
	boom := errors.New("boom")
	github := testFile(t, "auth:\n"+adminPassword+"  github:\n    clientId: Iv1.x\n    clientSecret: { env: TEST_AUTH_SECRET }\n")
	gh, _ := github.Auth.SignInByType(configfile.SignInGitHub)
	oidc := testFile(t, "auth:\n  oidc:\n    issuer: https://id.example.com\n    clientId: k\n    clientSecret: { env: TEST_AUTH_SECRET }\n"+
		"    roleMapping: 'claims.groups[0] == \"a\" ? \"admin\" : \"\"'\n")
	od, _ := oidc.Auth.SignInByType(configfile.SignInOIDC)
	oidcNone := testFile(t, "auth:\n  oidc:\n    issuer: https://id.example.com\n    clientId: k\n    clientSecret: { env: TEST_AUTH_SECRET }\n"+
		"    roleMapping: '\"kritik-admin\" in roles ? \"admin\" : \"\"'\n")
	on, _ := oidcNone.Auth.SignInByType(configfile.SignInOIDC)
	for _, tt := range []struct {
		name  string
		file  *configfile.File
		s     *configfile.SignIn
		facts Facts
		want  error
	}{
		{"a stranger on github", github, gh, githubFacts(&orgs{}, "mallory"), ErrNoGrant},
		{"the forge failing", github, gh, githubFacts(&orgs{err: boom}, "mallory"), boom},
		{"a mapping that fails", oidc, od, oidcFacts(), ErrRoleMapping},
		{"oidc placing nobody by default", oidcNone, on, oidcFacts("other"), ErrNoGrant},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := grant(context.Background(), tt.file, tt.s, Identity{Login: "mallory"}, tt.facts); !errors.Is(err, tt.want) {
				t.Fatalf("grant = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestGrantKey: a sign-in's key changes with what decides its grants, and
// with nothing else.
func TestGrantKey(t *testing.T) {
	base := "auth:\n  admin: { password: { env: TEST_AUTH_SECRET } }\n  github:\n    clientId: Iv1.x\n    clientSecret: { env: TEST_AUTH_SECRET }\n" +
		"    roleMapping: 'login == \"a\" ? \"admin\" : \"\"'\n"
	key := func(auth, provider string) string {
		t.Helper()
		k, ok := GrantKey(testFile(t, auth).Auth, provider)
		if !ok {
			t.Fatalf("GrantKey(%s) not configured", provider)
		}
		return k
	}
	gh, local := key(base, "github"), key(base, "local")
	if gh == local {
		t.Fatal("two sign-ins share a key")
	}
	if key(strings.Replace(base, "clientId: Iv1.x", "clientId: Iv1.y", 1), "github") != gh {
		t.Fatal("a client id change changed the key")
	}
	if key(strings.Replace(base, `login == "a"`, `login == "b"`, 1), "github") == gh {
		t.Fatal("a mapping change kept the key")
	}
	if key(strings.Replace(base, "admin: {", "admin: { user: root,", 1), "local") == local {
		t.Fatal("renaming the admin kept the key")
	}
	t.Setenv("TEST_AUTH_SECRET", "rotated")
	f := configfiletest.Load(t, base+connectionsYAML)
	if k, _ := GrantKey(f.Auth, "local"); k == local {
		t.Fatal("rotating the password kept the key")
	}
	if _, ok := GrantKey(f.Auth, "oidc"); ok {
		t.Fatal("an unconfigured sign-in has a key")
	}
}
