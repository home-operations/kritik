package configfile

import (
	"strings"

	"github.com/google/uuid"
)

// namespace roots every deterministic identifier kritika derives. Accounts
// and connections get their ids from their names so that any role can
// address them without a lookup that row-level security would forbid before
// the account is known: the webhook listener derives the account id from the
// repository owner a webhook names and opens the account transaction
// directly.
var namespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/home-operations/kritika"))

// ID is the account's stable identifier, derived from its forge and name.
func (a *Account) ID() string { return AccountID(a.Forge, a.Name) }

// Key is how the account is named across kritika: "<forge>/<name>",
// lowercased, since forge account names are not case sensitive.
func (a *Account) Key() string { return AccountKey(a.Forge, a.Name) }

// AccountKey is the key of the account name on forge.
func AccountKey(forge Forge, name string) string {
	return strings.ToLower(string(forge) + "/" + name)
}

// AccountID is the id of the account name on forge.
func AccountID(forge Forge, name string) string {
	return uuid.NewSHA1(namespace, []byte("account:"+AccountKey(forge, name))).String()
}

// ID is the connection's stable identifier, derived from its name.
func (i *Connection) ID() string {
	return uuid.NewSHA1(namespace, []byte("connection:"+i.Name)).String()
}

// RepositoryID is the stable identifier of a repository of an account,
// derived from the account's id and the repository's full name, lowercased
// since GitHub names are not case sensitive, so ingest can upsert it
// without first reading it back, however a name is spelled.
func RepositoryID(accountID, fullName string) string {
	return uuid.NewSHA1(namespace, []byte("repository:"+accountID+":"+strings.ToLower(fullName))).String()
}

// Slug is how the dashboard names the account in its URLs and lists:
// "<forge>/<name>", with the name as the configuration spells it.
func (a *Account) Slug() string { return string(a.Forge) + "/" + a.Name }
