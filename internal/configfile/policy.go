package configfile

import (
	"reflect"
	"strings"
)

// Scope is where the operator writes a repository setting.
type Scope string

// Operator scopes, broadest first.
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
	// RepoOwn is a setting only the file has.
	RepoOwn RepoRule = "own"
	// RepoTurnOff may only turn the setting off.
	RepoTurnOff RepoRule = "turnOff"
	// RepoTurnOn may only turn the setting on.
	RepoTurnOn RepoRule = "turnOn"
	// RepoAnd is ANDed with the operator's.
	RepoAnd RepoRule = "and"
	// RepoUnion is added to the operator's.
	RepoUnion RepoRule = "union"
	// RepoAppend follows the operator's.
	RepoAppend RepoRule = "append"
	// RepoChoose picks one value the operator's allow bound lists.
	RepoChoose RepoRule = "choose"
	// RepoSubset picks some of the values the operator's allow bound lists.
	RepoSubset RepoRule = "subset"
	// RepoAtMost is at most the operator's allow bound.
	RepoAtMost RepoRule = "atMost"
	// RepoReplace replaces the operator's; it grants nothing.
	RepoReplace RepoRule = "replace"
)

// Policy is one row of the table that says where a repository setting is
// written (ADR-0010 §2.4, §2.7): the scopes the operator writes it at, and
// what the repository's .kritik.yaml may do with it. The dashboard serves it
// for the UI to render from, and the keys the repository file takes follow
// it. Instance settings are the file's alone and not in it.
type Policy struct {
	// Key is the setting as the configuration spells it; a dotted key is
	// nested.
	Key        string   `json:"key"`
	Scopes     []Scope  `json:"scopes"`
	Repository RepoRule `json:"repository,omitempty"`
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

// Policies is the table.
var Policies = []Policy{
	{Key: "enabled", Scopes: []Scope{ScopeRepository}, Repository: RepoTurnOff},
	{Key: "filter", Scopes: everyScope, Repository: RepoAnd},
	{Key: "ignore", Scopes: everyScope, Repository: RepoUnion},
	{Key: "skip.onlyPaths", Scopes: []Scope{}, Repository: RepoOwn},
	{Key: "forks", Scopes: everyScope},
	{Key: "models.review", Scopes: everyScope, Repository: RepoChoose},
	{Key: "models.fallback", Scopes: everyScope, Repository: RepoChoose},
	{Key: "mode", Scopes: everyScope, Repository: RepoChoose},
	{Key: keyMaxSteps, Scopes: everyScope, Repository: RepoAtMost},
	{Key: keyMaxToolOutputBytes, Scopes: everyScope, Repository: RepoAtMost},
	{Key: keyMaxTokens, Scopes: everyScope, Repository: RepoAtMost},
	{Key: keyTimeout, Scopes: everyScope, Repository: RepoAtMost},
	{Key: "agent.commands", Scopes: everyScope, Repository: RepoSubset},
	{Key: "agent.commandTimeout", Scopes: everyScope},
	{Key: "settle", Scopes: everyScope, Repository: RepoAtMost},
	{Key: "incremental.maxDeltaFiles", Scopes: everyScope},
	{Key: "review.instructions", Scopes: everyScope, Repository: RepoAppend},
	{Key: "review.requireSuggestedFix", Scopes: everyScope, Repository: RepoTurnOn},
	{Key: "review.templates", Scopes: everyScope, Repository: RepoReplace},
	{Key: "review.context", Scopes: everyScope, Repository: RepoAppend},
	{Key: "review.minSeverity", Scopes: everyScope, Repository: RepoReplace},
	{Key: "review.inlineComments", Scopes: everyScope, Repository: RepoReplace},
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
