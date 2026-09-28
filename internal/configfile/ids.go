package configfile

import "github.com/google/uuid"

// namespace roots every deterministic identifier kritik derives. Tenants and
// connections get their ids from their names so that any role can address
// them without a lookup that row-level security would forbid before the
// tenant is known: the ingest role derives the tenant id from the
// connection it was called for and opens the tenant transaction directly.
var namespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/home-operations/kritik"))

// ID is the tenant's stable identifier, derived from its slug.
func (t *Tenant) ID() string {
	return uuid.NewSHA1(namespace, []byte("tenant:"+t.Slug)).String()
}

// ID is the connection's stable identifier, derived from its name.
func (i *Connection) ID() string {
	return uuid.NewSHA1(namespace, []byte("connection:"+i.Name)).String()
}

// RepositoryID is the stable identifier of a repository under a
// connection, derived from both names, so ingest can upsert it without
// first reading it back.
func RepositoryID(connectionID, fullName string) string {
	return uuid.NewSHA1(namespace, []byte("repository:"+connectionID+":"+fullName)).String()
}
