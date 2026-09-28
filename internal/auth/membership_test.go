package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

// orgs is a fake Membership answering from a fixed table, counting calls.
type orgs struct {
	roles map[string]Role
	err   error
	calls []string
}

func (o *orgs) membership() Membership {
	return func(_ context.Context, org string) (Role, error) {
		o.calls = append(o.calls, org)
		if o.err != nil {
			return "", o.err
		}
		return o.roles[strings.ToLower(org)], nil
	}
}

func tenant(slug string, installs ...configfile.Installation) configfile.Tenant {
	return configfile.Tenant{Slug: slug, Installations: installs}
}

func install(account string) configfile.Installation {
	return configfile.Installation{Name: account + "-bot", Forge: configfile.ForgeGitHub, Accounts: []string{account}}
}

func TestResolve(t *testing.T) {
	file := &configfile.File{Tenants: []configfile.Tenant{
		tenant("personal", install("Alice")),
		tenant("org", install("acme")),
		tenant("adminorg", install("widgets")),
		tenant("several", configfile.Installation{Name: "several-bot", Forge: configfile.ForgeGitHub, Accounts: []string{"nobody", "Widgets"}}),
	}}
	id := func(t *testing.T, slug string) string {
		t.Helper()
		for i := range file.Tenants {
			if file.Tenants[i].Slug == slug {
				return file.Tenants[i].ID()
			}
		}
		t.Fatalf("no tenant %s", slug)
		return ""
	}
	github := configfile.SignIn{Name: "gh", Type: configfile.SignInGitHub}
	tests := []struct {
		name   string
		signIn configfile.SignIn
		login  string
		roles  map[string]Role
		want   map[string]Role
	}{
		{
			name: "personal account is admin, org member and org admin", signIn: github, login: "alice",
			roles: map[string]Role{"acme": RoleMember, "widgets": RoleAdmin},
			want:  map[string]Role{"personal": RoleAdmin, "org": RoleMember, "adminorg": RoleAdmin, "several": RoleAdmin},
		},
		{
			name: "not a member of anything", signIn: github, login: "bob",
			want: map[string]Role{},
		},
		{
			name: "oidc resolves nothing", login: "alice",
			signIn: configfile.SignIn{Name: "corp", Type: configfile.SignInOIDC, Issuer: "https://id.example.com"},
			roles:  map[string]Role{"acme": RoleAdmin},
			want:   map[string]Role{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &orgs{roles: tt.roles}
			grants, err := Resolve(context.Background(), file, tt.signIn, Identity{Login: tt.login}, o.membership())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got := map[string]Role{}
			for _, g := range grants {
				if _, dup := got[g.TenantID]; dup {
					t.Fatalf("tenant %s granted twice", g.TenantID)
				}
				got[g.TenantID] = g.Role
			}
			want := map[string]Role{}
			for slug, r := range tt.want {
				want[id(t, slug)] = r
			}
			if len(got) != len(want) {
				t.Fatalf("grants = %v, want %v", got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("grants = %v, want %v", got, want)
				}
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

func TestResolveErrors(t *testing.T) {
	file := &configfile.File{Tenants: []configfile.Tenant{tenant("org", install("acme"))}}
	signIn := configfile.SignIn{Name: "gh", Type: configfile.SignInGitHub}
	boom := errors.New("boom")
	o := &orgs{err: boom}
	if _, err := Resolve(context.Background(), file, signIn, Identity{Login: "alice"}, o.membership()); !errors.Is(err, boom) {
		t.Fatalf("Resolve error = %v, want %v", err, boom)
	}
	grants, err := Resolve(context.Background(), file, signIn, Identity{Login: "acme"}, nil)
	if err != nil || len(grants) != 1 || grants[0].Role != RoleAdmin {
		t.Fatalf("Resolve with nil membership = %v, %v; want the personal-account admin grant", grants, err)
	}
}

func TestIsOperator(t *testing.T) {
	web := configfile.Web{
		SignIn: []configfile.SignIn{
			{Name: "gh", Type: configfile.SignInGitHub},
			{Name: "corp", Type: configfile.SignInOIDC},
		},
		Operators: []string{"gh:Alice", "corp:Sub-123", "email:Ops@Example.com", "gone:carol"},
	}
	tests := []struct {
		name string
		id   Identity
		want bool
	}{
		{"forge login, case-insensitive", Identity{Provider: "gh", Login: "ALICE", Subject: "1"}, true},
		{"same login on another provider", Identity{Provider: "corp", Login: "alice", Subject: "1"}, false},
		{"oidc subject, exact", Identity{Provider: "corp", Subject: "Sub-123", Login: "x"}, true},
		{"oidc subject case differs", Identity{Provider: "corp", Subject: "sub-123"}, false},
		{"oidc matched by login is not enough", Identity{Provider: "corp", Subject: "zzz", Login: "Sub-123"}, false},
		{"verified email, case-insensitive", Identity{Provider: "corp", Login: "z", Email: "ops@example.COM", EmailVerified: true}, true},
		{"unverified email", Identity{Provider: "corp", Login: "z", Email: "ops@example.com"}, false},
		{"operator on a sign-in no longer configured", Identity{Provider: "gone", Login: "carol"}, false},
		{"nobody", Identity{Provider: "gh", Login: "bob"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOperator(web, tt.id); got != tt.want {
				t.Fatalf("IsOperator(%+v) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestRoleMax(t *testing.T) {
	roles := []Role{"", RoleMember, RoleAdmin}
	for _, a := range roles {
		for _, b := range roles {
			got := maxRole(a, b)
			want := a
			if slices.Index(roles, b) > slices.Index(roles, a) {
				want = b
			}
			if got != want {
				t.Fatalf("maxRole(%q, %q) = %q, want %q", a, b, got, want)
			}
		}
	}
}
