package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// Role is what a membership lets an account do on a tenant.
type Role = store.Role

// Membership roles.
const (
	RoleAdmin  = store.RoleAdmin
	RoleMember = store.RoleMember
)

// Grant is a role on one tenant.
type Grant = store.Grant

// Membership answers, with the signed-in user's own token, what role the
// user holds in a forge organization: RoleAdmin, RoleMember, or "" when the
// user is not an active member.
type Membership func(ctx context.Context, org string) (Role, error)

// Resolve derives the tenants a forge sign-in grants: for every account a
// tenant installation on the same forge as signIn serves, the user is admin
// when the account is their own, otherwise whatever role m reports for that
// account as an organization. A tenant gets the highest role among its
// matching installations' accounts. An OIDC sign-in matches no installation
// and resolves nothing.
func Resolve(ctx context.Context, file *configfile.File, signIn configfile.SignIn, id Identity, m Membership) ([]Grant, error) {
	checked := map[string]Role{}
	var grants []Grant
	for ti := range file.Tenants {
		t := &file.Tenants[ti]
		var role Role
		for _, in := range t.Installations {
			if string(in.Forge) != string(signIn.Type) {
				continue
			}
			for _, account := range in.Accounts {
				r, err := accountRole(ctx, id.Login, account, m, checked)
				if err != nil {
					return nil, fmt.Errorf("auth: tenant %s: %w", t.Slug, err)
				}
				role = maxRole(role, r)
			}
		}
		if role != "" {
			grants = append(grants, Grant{TenantID: t.ID(), Role: role})
		}
	}
	return grants, nil
}

// accountRole is the user's role on one forge account, asking m about each
// organization once per Resolve.
func accountRole(ctx context.Context, login, account string, m Membership, checked map[string]Role) (Role, error) {
	if account == "" {
		return "", nil
	}
	if strings.EqualFold(login, account) {
		return RoleAdmin, nil
	}
	if m == nil {
		return "", nil
	}
	key := strings.ToLower(account)
	if r, ok := checked[key]; ok {
		return r, nil
	}
	r, err := m(ctx, account)
	if err != nil {
		return "", err
	}
	if !r.Valid() {
		r = ""
	}
	checked[key] = r
	return r, nil
}

func maxRole(a, b Role) Role {
	if a == RoleAdmin || b == RoleAdmin {
		return RoleAdmin
	}
	if a == RoleMember || b == RoleMember {
		return RoleMember
	}
	return ""
}

// IsOperator reports whether id is on the file's operator allowlist:
// "<sign-in name>:<login>" for a GitHub sign-in, compared
// without case as forge logins are; "<sign-in name>:<subject>" for OIDC,
// exact, since a subject is opaque; or "email:<address>", which only a
// provider-verified email matches. An entry naming a sign-in the file no
// longer declares matches nobody.
func IsOperator(web configfile.Web, id Identity) bool {
	signIn, known := web.SignInByName(id.Provider)
	for _, op := range web.Operators {
		kind, subject, ok := strings.Cut(op, ":")
		if !ok || subject == "" {
			continue
		}
		switch {
		case kind == configfile.OperatorEmail:
			if id.EmailVerified && id.Email != "" && strings.EqualFold(subject, id.Email) {
				return true
			}
		case !known || kind != id.Provider:
		case signIn.Type == configfile.SignInOIDC:
			if subject == id.Subject {
				return true
			}
		default:
			if id.Login != "" && strings.EqualFold(subject, id.Login) {
				return true
			}
		}
	}
	return false
}
