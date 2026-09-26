package webapi

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// fieldPolicies is the policy table as p meets it on a tenant whose
// configuration it may change (editable) or not: a setting the table
// keeps for operators is editable only by one.
func fieldPolicies(p *auth.Principal, editable bool) []FieldPolicy {
	out := make([]FieldPolicy, len(configfile.Policies))
	for i, pol := range configfile.Policies {
		out[i] = FieldPolicy{Policy: pol, Editable: editable && (p.Operator || pol.TenantAdmin)}
	}
	return out
}

// operatorOnlyChange is the key of the first setting the policy table
// keeps for operators that differs between the stored tenant and its
// replacement, "" when none does. Repositories are matched by
// installation and name; a removed repository that set any of them
// changes them too, back to what it inherits.
func operatorOnlyChange(prev, next *configfile.Tenant) string {
	if k := operatorOnlyKey(configfile.ScopeTenant, prev, next); k != "" {
		return k
	}
	key := func(r *configfile.Repository) string { return r.Installation + "\x00" + r.Name }
	byKey := map[string]*configfile.Repository{}
	for i := range prev.Repositories {
		byKey[key(&prev.Repositories[i])] = &prev.Repositories[i]
	}
	var zero configfile.Repository
	for i := range next.Repositories {
		n := &next.Repositories[i]
		p := &zero
		if r, ok := byKey[key(n)]; ok {
			p = r
		}
		delete(byKey, key(n))
		if k := operatorOnlyKey(configfile.ScopeRepository, p, n); k != "" {
			return "repositories[" + strconv.Itoa(i) + "]." + k
		}
	}
	for _, p := range byKey {
		if operatorOnlyKey(configfile.ScopeRepository, p, &zero) != "" {
			return "repositories"
		}
	}
	return ""
}

// operatorOnlyKey is the top-level key of the first setting only an
// operator may write at scope that a and b, that scope's specs, write
// differently; "" when none.
func operatorOnlyKey(scope configfile.Scope, a, b any) string {
	for _, p := range configfile.Policies {
		if p.TenantAdmin || !slices.Contains(p.Scopes, scope) {
			continue
		}
		va, _ := configfile.SpecValue(a, p.Key)
		vb, _ := configfile.SpecValue(b, p.Key)
		if !sameSetting(va, vb) {
			top, _, _ := strings.Cut(p.Key, ".")
			return top
		}
	}
	return ""
}

// sameSetting reports whether two scopes write a setting alike, compared
// as the configuration spells them, where an empty block is no block.
func sameSetting(a, b any) bool {
	return settingText(a) == settingText(b)
}

func settingText(v any) string {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	if s := string(raw); s != "{}\n" {
		return s
	}
	return "null\n"
}

// leavesNoAdmin reports whether changing an admin's own invite grant to
// after ("" deletes it) would leave the tenant with no admin but
// operators. grants are the account's grants before the change; others
// are the identities of every other account that is an admin.
func leavesNoAdmin(web configfile.Web, grants []store.MemberGrant, after auth.Role, others []store.AdminIdentity) bool {
	var was, is bool
	for _, g := range grants {
		was = was || g.Role == store.RoleAdmin
		role := g.Role
		if g.Source == store.SourceInvite {
			role = after
		}
		is = is || role == store.RoleAdmin
	}
	if !was || is {
		return false
	}
	operators := map[string]bool{}
	for _, o := range others {
		if auth.IsOperator(web, auth.Identity(o.Identity)) {
			operators[o.AccountID] = true
		}
	}
	for _, o := range others {
		if !operators[o.AccountID] {
			return false
		}
	}
	return true
}
