package webapi

import (
	"reflect"
	"slices"
	"strconv"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// operatorOnlyFields are the spec fields only an operator may change on a
// dashboard tenant (ADR-0009 §2.15).
var operatorOnlyFields = []string{
	"models", "forks", "runner", "limits", "repositories[].agent", "repositories[].mode", "repositories[].incremental",
}

// operatorOnlyChange is the path of the first operator-only field that
// differs between the stored tenant and its replacement, "" when none
// does. Repositories are matched by name; a removed repository that set
// any of them changes them too, back to their defaults.
func operatorOnlyChange(prev, next *configfile.Tenant) string {
	if prev.Models != next.Models {
		return "models"
	}
	if !ptrEqual(prev.Forks, next.Forks) {
		return "forks"
	}
	if !runnerEqual(prev.Runner, next.Runner) {
		return "runner"
	}
	if prev.Limits != next.Limits {
		return "limits"
	}
	byName := map[string]*configfile.Repository{}
	for i := range prev.Repositories {
		byName[prev.Repositories[i].Name] = &prev.Repositories[i]
	}
	var zero configfile.Repository
	for i := range next.Repositories {
		n := &next.Repositories[i]
		p, ok := byName[n.Name]
		if !ok {
			p = &zero
		}
		delete(byName, n.Name)
		if modeOf(p.Mode) != modeOf(n.Mode) {
			return "repositories[" + strconv.Itoa(i) + "].mode"
		}
		if !agentEqual(p.Agent, n.Agent) {
			return "repositories[" + strconv.Itoa(i) + "].agent"
		}
		if !ptrEqual(p.Incremental.MaxDeltaFiles, n.Incremental.MaxDeltaFiles) {
			return "repositories[" + strconv.Itoa(i) + "].incremental"
		}
	}
	for _, p := range byName {
		if modeOf(p.Mode) != modeOf(zero.Mode) || !agentEqual(p.Agent, zero.Agent) || p.Incremental.MaxDeltaFiles != nil {
			return "repositories"
		}
	}
	return ""
}

// modeOf is the mode a repository resolves to.
func modeOf(m configfile.ReviewMode) configfile.ReviewMode {
	if m == "" {
		return configfile.ReviewSingle
	}
	return m
}

func runnerEqual(a, b *configfile.Runner) bool {
	var zero configfile.Runner
	if a == nil {
		a = &zero
	}
	if b == nil {
		b = &zero
	}
	if a.ActiveDeadlineSeconds != b.ActiveDeadlineSeconds {
		return false
	}
	if len(a.Resources) == 0 && len(b.Resources) == 0 {
		return true
	}
	return reflect.DeepEqual(a.Resources, b.Resources)
}

func agentEqual(a, b configfile.Agent) bool {
	return ptrEqual(a.MaxSteps, b.MaxSteps) && ptrEqual(a.MaxToolOutputBytes, b.MaxToolOutputBytes) &&
		ptrEqual(a.MaxTokens, b.MaxTokens) && ptrEqual(a.Timeout, b.Timeout) && ptrEqual(a.CommandTimeout, b.CommandTimeout) &&
		slices.Equal(a.Commands, b.Commands)
}

func ptrEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
