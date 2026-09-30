package configfile

import (
	"reflect"
	"strings"
)

// Scope is where an admin writes a repository setting.
type Scope string

// Admin scopes, broadest first.
const (
	ScopeDefaults   Scope = "defaults"
	ScopeAccount    Scope = "account"
	ScopeRepository Scope = "repository"
)

// RepoRule is what a repository's own .kritik.yaml may do with a setting.
type RepoRule string

// Repository rules. The empty rule is none: the file cannot name the
// setting at all.
const (
	// RepoTurnOff may only turn the setting off.
	RepoTurnOff RepoRule = "turnOff"
	// RepoTurnOn may only turn the setting on.
	RepoTurnOn RepoRule = "turnOn"
	// RepoAnd is ANDed with the admin's.
	RepoAnd RepoRule = "and"
	// RepoUnion is added to the admin's.
	RepoUnion RepoRule = "union"
	// RepoAppend follows the admin's.
	RepoAppend RepoRule = "append"
	// RepoChoose picks one value an admin's allow bound lists.
	RepoChoose RepoRule = "choose"
	// RepoSubset picks some of the values an admin's allow bound lists.
	RepoSubset RepoRule = "subset"
	// RepoAtMost is at most an admin's allow bound.
	RepoAtMost RepoRule = "atMost"
	// RepoReplace replaces the admin's; it grants nothing.
	RepoReplace RepoRule = "replace"
)

// Policy is one row of the table that says where a repository setting is
// written (ADR-0010 §2.4, §2.7): the scopes an admin writes it at, and what
// the repository's .kritik.yaml may do with it. Sources reports where each
// setting it lists comes from, and the keys the repository file takes
// follow it. Instance-wide settings are not in it.
type Policy struct {
	// Key is the setting as the configuration spells it; a dotted key is
	// nested.
	Key        string
	Scopes     []Scope
	Repository RepoRule
}

var (
	everyScope    = []Scope{ScopeDefaults, ScopeAccount, ScopeRepository}
	accountScopes = []Scope{ScopeDefaults, ScopeAccount}
)

// The agent limits' keys.
const (
	keyMaxSteps           = "agent.maxSteps"
	keyMaxToolOutputBytes = "agent.maxToolOutputBytes"
	keyMaxTokens          = "agent.maxTokens"
	keyTimeout            = "agent.timeout"
)

// The keys the file's defaults set as well (instance.go).
const (
	keyMode     = "mode"
	keyFeedback = "review.feedback"
	keyForks    = "forks"
	keySettle   = "settle"
)

// Policies is the table.
var Policies = []Policy{
	{Key: "enabled", Scopes: everyScope, Repository: RepoTurnOff},
	{Key: "filter", Scopes: everyScope, Repository: RepoAnd},
	{Key: "ignore", Scopes: everyScope, Repository: RepoUnion},
	{Key: keyForks, Scopes: everyScope},
	{Key: "models.review", Scopes: everyScope, Repository: RepoChoose},
	{Key: "models.fallback", Scopes: everyScope, Repository: RepoChoose},
	{Key: keyMode, Scopes: everyScope, Repository: RepoChoose},
	{Key: keyMaxSteps, Scopes: everyScope, Repository: RepoAtMost},
	{Key: keyMaxToolOutputBytes, Scopes: everyScope, Repository: RepoAtMost},
	{Key: keyMaxTokens, Scopes: everyScope, Repository: RepoAtMost},
	{Key: keyTimeout, Scopes: everyScope, Repository: RepoAtMost},
	{Key: "agent.commands", Scopes: everyScope, Repository: RepoSubset},
	{Key: "agent.commandTimeout", Scopes: everyScope},
	{Key: keySettle, Scopes: everyScope, Repository: RepoAtMost},
	{Key: "incremental.maxDeltaFiles", Scopes: everyScope},
	{Key: "review.requireSuggestedFix", Scopes: everyScope, Repository: RepoTurnOn},
	{Key: "review.templates", Scopes: everyScope, Repository: RepoReplace},
	{Key: "review.context", Scopes: everyScope, Repository: RepoAppend},
	{Key: "review.rules", Scopes: everyScope, Repository: RepoAppend},
	{Key: "review.inlineComments", Scopes: everyScope, Repository: RepoReplace},
	{Key: keyFeedback, Scopes: everyScope, Repository: RepoReplace},
	{Key: "review.agentFiles", Scopes: everyScope, Repository: RepoReplace},
	{Key: "allow", Scopes: everyScope},
	{Key: "limits", Scopes: accountScopes},
	{Key: "runner", Scopes: accountScopes},
}

// SpecValue is the value spec, one scope's settings (a *Defaults, *Account or
// *Repository), writes for key, found by the names the configuration
// spells, and whether the scope has the key at all. A nested key under an
// unset block is that block's zero value.
func SpecValue(spec any, key string) (any, bool) {
	v := reflect.ValueOf(spec)
	for part := range strings.SplitSeq(key, ".") {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v = reflect.Zero(v.Type().Elem())
			} else {
				v = v.Elem()
			}
		}
		if v.Kind() != reflect.Struct {
			return nil, false
		}
		f, ok := fieldByKey(v, part)
		if !ok {
			return nil, false
		}
		v = f
	}
	return v.Interface(), true
}

// fieldByKey finds the field of struct v a configuration key names,
// looking into inline blocks.
func fieldByKey(v reflect.Value, key string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		name, opts, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if opts == "inline" {
			if f, ok := fieldByKey(v.Field(i), key); ok {
				return f, true
			}
			continue
		}
		if name != "" && name == key {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}
