package rolemap

import (
	"maps"
	"strings"
	"testing"
)

func TestCompileRejects(t *testing.T) {
	for _, tt := range []struct {
		name string
		kind Kind
		expr string
		want string
	}{
		{"syntax", OIDC, `"admin" ?`, "Syntax error"},
		{"a variable of the other kind", OIDC, `login == "a" ? "admin" : ""`, "undeclared reference to 'login'"},
		{"a boolean", GitHub, `"org-1" in orgs`, "must yield a role or a map"},
		{"a number", GitHub, `1`, "must yield a role or a map"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Compile(tt.kind, tt.expr); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Compile = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestEval(t *testing.T) {
	oidc := func(roles ...string) map[string]any {
		return map[string]any{"claims": map[string]any{"sub": "x"}, "roles": roles}
	}
	github := func(login string, orgs ...string) map[string]any {
		return map[string]any{"login": login, "email": "", "orgs": orgs, "teams": []string{}}
	}
	for _, tt := range []struct {
		name string
		kind Kind
		expr string
		vars map[string]any
		want Result
	}{
		{"an admin by role", OIDC, `"kritika-admin" in roles ? "admin" : ("kritika-user" in roles ? "member" : "")`,
			oidc("kritika-admin"), Result{Role: RoleAdmin}},
		{"a member by role", OIDC, `"kritika-admin" in roles ? "admin" : ("kritika-user" in roles ? "member" : "")`,
			oidc("kritika-user"), Result{Role: RoleMember}},
		{"nobody", OIDC, `"kritika-admin" in roles ? "admin" : ""`, oidc("other"), Result{}},
		{"a claim", OIDC, `claims.sub == "x" ? "member" : ""`, oidc(), Result{Role: RoleMember}},
		{"an admin by login", GitHub, `login == "user-1" ? "admin" : ""`, github("user-1"), Result{Role: RoleAdmin}},
		{"accounts by map", GitHub, `{"github/Org-2": "member", "github/org-3": ""}`, github("x"),
			Result{Accounts: map[string]bool{"github/org-2": true}}},
		{"every account", OIDC, `{"*": "member"}`, oidc(), Result{Accounts: map[string]bool{AllAccounts: true}}},
		{"a role or a map", GitHub, `"org-1" in orgs ? "admin" : dyn({"github/org-2": "member"})`, github("x", "org-2"),
			Result{Accounts: map[string]bool{"github/org-2": true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Compile(tt.kind, tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Eval(tt.vars)
			if err != nil {
				t.Fatal(err)
			}
			if got.Role != tt.want.Role || !maps.Equal(got.Accounts, tt.want.Accounts) {
				t.Fatalf("Eval = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEvalRejects(t *testing.T) {
	for _, tt := range []struct {
		name string
		expr string
		want string
	}{
		{"an unknown role", `"owner"`, `"owner" is not a role`},
		{"an admin account", `{"github/org-2": "admin"}`, `only takes "member"`},
		{"a missing claim", `claims.groups[0] == "a" ? "admin" : ""`, "no such key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Compile(OIDC, tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Eval(map[string]any{"claims": map[string]any{}, "roles": []string{}}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Eval = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}
