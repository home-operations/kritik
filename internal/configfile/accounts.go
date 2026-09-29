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

// validateAccountNames requires at least one account, each an account's
// name and none listed twice, in any case.
func validateAccountNames(accounts []string, where string) error {
	if len(accounts) == 0 {
		return fmt.Errorf("configfile: %s.accounts must list at least one account", where)
	}
	seen := make(map[string]bool, len(accounts))
	for i, a := range accounts {
		if err := checkAccountName(fmt.Sprintf("%s.accounts[%d]", where, i), a); err != nil {
			return err
		}
		key := strings.ToLower(a)
		if seen[key] {
			return fmt.Errorf("configfile: %s.accounts[%d] %q is listed twice", where, i, a)
		}
		seen[key] = true
	}
	return nil
}

// checkAccountName requires name to be an account's name on the forge: one
// segment of a repository's full name.
func checkAccountName(where, name string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/ ") {
		return fmt.Errorf("configfile: %s %q must be the account's name on the forge", where, name)
	}
	return nil
}
