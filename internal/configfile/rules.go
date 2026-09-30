package configfile

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

// Rule is a check a review makes, written in the configuration
// (ADR-0018), by an id findings cite it by and narrower scopes replace it
// by. With Paths it applies only when a changed path matches one of them.
type Rule struct {
	ID    string   `yaml:"id" json:"id"`
	Rule  string   `yaml:"rule" json:"rule"`
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// MaxRuleChars bounds one rule's text.
const MaxRuleChars = 2000

// ruleIDRe is what a rule's id may be.
var ruleIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// CheckRules rejects a list of rules with an id that is not valid or is
// listed twice, a blank or overlong rule, or a glob that is not valid. The
// error starts with the rule's place in the list, as rules[i].
func CheckRules(rules []Rule) error {
	for i, r := range rules {
		if !ruleIDRe.MatchString(r.ID) {
			return fmt.Errorf("rules[%d].id %q must be lowercase letters, digits and hyphens, "+
				"starting with a letter or digit, at most 64 characters", i, r.ID)
		}
		if slices.ContainsFunc(rules[:i], func(o Rule) bool { return o.ID == r.ID }) {
			return fmt.Errorf("rules[%d].id %q is listed twice", i, r.ID)
		}
		if strings.TrimSpace(r.Rule) == "" {
			return fmt.Errorf("rules[%d].rule is required", i)
		}
		if n := utf8.RuneCountInString(r.Rule); n > MaxRuleChars {
			return fmt.Errorf("rules[%d].rule is %d characters, over the %d allowed", i, n, MaxRuleChars)
		}
		for j, g := range r.Paths {
			if strings.TrimSpace(g) == "" || !doublestar.ValidatePattern(g) {
				return fmt.Errorf("rules[%d].paths[%d] %q is not a valid glob", i, j, g)
			}
		}
	}
	return nil
}

// RuleScopes says, by id, which scope writes each rule Settings(a,
// fullName) runs: the defaults, the account, or its entry for the
// repository.
func (f *File) RuleScopes(a *Account, fullName string) map[string]Scope {
	out := map[string]Scope{}
	for _, r := range f.Defaults.Review.Rules {
		out[r.ID] = ScopeDefaults
	}
	for _, r := range a.Review.Rules {
		out[r.ID] = ScopeAccount
	}
	if e := a.Repository(fullName); e != nil {
		for _, r := range e.Review.Rules {
			out[r.ID] = ScopeRepository
		}
	}
	return out
}

// WithRules is rules with more laid over them: one of more with an id
// rules already has replaces that rule where it stands, and the rest
// follow in order.
func WithRules(rules, more []Rule) []Rule {
	if len(more) == 0 {
		return rules
	}
	out := slices.Clone(rules)
	for _, r := range more {
		if i := slices.IndexFunc(out, func(o Rule) bool { return o.ID == r.ID }); i >= 0 {
			out[i] = r
		} else {
			out = append(out, r)
		}
	}
	return out
}
