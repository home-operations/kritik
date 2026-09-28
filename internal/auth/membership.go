package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/rolemap"
	"github.com/home-operations/kritik/internal/store"
)

// Role is what a session lets its user do.
type Role = store.Role

// Session roles.
const (
	RoleAdmin  = store.RoleAdmin
	RoleMember = store.RoleMember
)

// Membership answers, with the signed-in user's own token, whether the
// user is an active member of a forge organization.
type Membership func(ctx context.Context, org string) (bool, error)

// Errors deciding a grant, each a refused sign-in.
var (
	// ErrNoGrant is a sign-in that places the person nowhere: neither the
	// role mapping nor the forge gives them any access.
	ErrNoGrant = errors.New("auth: the sign-in grants no access")
	// ErrRoleMapping is a role mapping that failed to evaluate.
	ErrRoleMapping = errors.New("auth: role mapping")
)

// grant decides what a sign-in allows (ADR-0014 §2.5): the sign-in's role
// mapping, evaluated over facts, and for a forge the served accounts the
// person belongs to; where both speak, the higher role wins. The grant's
// key binds the session to the configuration that decided it.
func grant(ctx context.Context, file *configfile.File, s *configfile.SignIn, id Identity, facts Facts) (store.SessionGrant, error) {
	var mapped rolemap.Result
	if prg := s.Mapping(); prg != nil {
		vars, err := facts.MappingVars(ctx)
		if err != nil {
			return store.SessionGrant{}, err
		}
		if mapped, err = prg.Eval(vars); err != nil {
			return store.SessionGrant{}, fmt.Errorf("%w: %w", ErrRoleMapping, err)
		}
	}
	g := store.SessionGrant{Role: RoleMember}
	switch {
	case mapped.Role == rolemap.RoleAdmin:
		g.Role = RoleAdmin
	case mapped.Role == rolemap.RoleMember || mapped.Accounts[rolemap.AllAccounts]:
		g.AllAccounts = true
	default:
		accounts := map[string]bool{}
		for a := range mapped.Accounts {
			accounts[a] = true
		}
		if facts.Membership != nil {
			forge, err := forgeAccounts(ctx, file, configfile.Forge(s.Type()), id.Login, facts.Membership)
			if err != nil {
				return store.SessionGrant{}, err
			}
			for _, a := range forge {
				accounts[a] = true
			}
		}
		for a := range accounts {
			g.Accounts = append(g.Accounts, a)
		}
		slices.Sort(g.Accounts)
		switch {
		case len(g.Accounts) > 0:
		case s.Type() == configfile.SignInOIDC && s.MembersByDefault():
			g.AllAccounts = true
		default:
			return store.SessionGrant{}, ErrNoGrant
		}
	}
	g.Key, _ = GrantKey(file.Auth, string(s.Type()))
	return g, nil
}

// forgeAccounts lists the accounts the running connections on forge serve
// that the person with login belongs to: their own login, and each
// organization m reports them an active member of, as account keys.
func forgeAccounts(ctx context.Context, file *configfile.File, forge configfile.Forge, login string, m Membership) ([]string, error) {
	var out []string
	for i := range file.Accounts {
		a := &file.Accounts[i]
		if a.Forge != forge || a.Name == "" {
			continue
		}
		ok := strings.EqualFold(login, a.Name)
		if !ok {
			var err error
			if ok, err = m(ctx, a.Name); err != nil {
				return nil, err
			}
		}
		if ok {
			out = append(out, a.Key())
		}
	}
	return out, nil
}

// GrantKey fingerprints what decides a sign-in's grants, the key a session
// through provider must carry to be honoured under a: for the local
// admin its name and password, and for a provider its role mapping and
// what the mapping reads. A session whose sign-in's key has changed since
// is not honoured, so editing a mapping, or rotating the password, ends
// the sessions it granted. ok is false when the sign-in is not configured.
func GrantKey(a configfile.Auth, provider string) (key string, ok bool) {
	var parts []string
	if configfile.SignInType(provider) == configfile.SignInLocal {
		user, password, ok := a.AdminUser()
		if !ok {
			return "", false
		}
		pw := sha256.Sum256([]byte(password.Value()))
		parts = []string{user, hex.EncodeToString(pw[:])}
	} else {
		s, ok := a.SignInByType(configfile.SignInType(provider))
		if !ok {
			return "", false
		}
		parts = []string{s.RoleMapping, s.RolesClaim, s.DefaultRole}
	}
	h := sha256.New()
	h.Write([]byte(provider))
	for _, p := range parts {
		// Length-prefixed, so no two different part lists hash alike; a
		// hash's Write never fails.
		_, _ = fmt.Fprintf(h, "\x00%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil)), true
}
