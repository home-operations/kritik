package configfile

import (
	"fmt"
	"slices"
	"strings"
)

// Serves reports whether account is one the connection serves.
func (i *Connection) Serves(account string) bool {
	return slices.ContainsFunc(i.Accounts, func(a string) bool { return strings.EqualFold(a, account) })
}

// validateAccountNames requires at least one account, none blank and none
// listed twice, in any case.
func validateAccountNames(accounts []string, where string) error {
	if len(accounts) == 0 {
		return fmt.Errorf("configfile: %s.accounts must list at least one account", where)
	}
	seen := make(map[string]bool, len(accounts))
	for i, a := range accounts {
		key := strings.ToLower(a)
		switch {
		case strings.TrimSpace(a) == "":
			return fmt.Errorf("configfile: %s.accounts[%d] is empty", where, i)
		case seen[key]:
			return fmt.Errorf("configfile: %s.accounts[%d] %q is listed twice", where, i, a)
		}
		seen[key] = true
	}
	return nil
}
