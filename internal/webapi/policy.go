package webapi

import "github.com/home-operations/kritik/internal/configfile"

// fieldPolicies is the policy table on a tenant whose configuration the
// caller may change (editable) or not: an admin changes every setting of a
// dashboard tenant, and nobody else changes any.
func fieldPolicies(editable bool) []FieldPolicy {
	out := make([]FieldPolicy, len(configfile.Policies))
	for i, pol := range configfile.Policies {
		out[i] = FieldPolicy{Policy: pol, Editable: editable}
	}
	return out
}
