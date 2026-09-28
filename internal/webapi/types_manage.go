package webapi

import (
	"encoding/json"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
)

// The management API's JSON shapes; internal/web/src/lib/types.ts mirrors
// these too.

// Meta is what the dashboard needs before anyone signs in.
type Meta struct {
	Version string `json:"version"`
	// Management is whether dashboard accounts can be written: a sealing
	// key is configured.
	Management bool                `json:"management"`
	SignIn     []auth.ProviderInfo `json:"signIn"`
	WebURL     string              `json:"webUrl"`
}

// AccountConfig is an account's spec as the principal may see it: every
// secret is {"set": bool}. Revision is null for a file account. Policy is
// the policy table as the principal meets it here, for the dashboard to
// render which settings it may change.
type AccountConfig struct {
	ManagedBy configfile.Origin `json:"managedBy"`
	Revision  *int64            `json:"revision"`
	Editable  bool              `json:"editable"`
	Policy    []FieldPolicy     `json:"policy"`
	Inherited Inherited         `json:"inherited"`
	Spec      json.RawMessage   `json:"spec"`
}

// Inherited is what an account's settings resolve to where its spec leaves a
// field out: Account for the account's own fields, from the defaults, and
// Repository for its repository entries, which inherit the account's. The
// sources say where each value comes from, by the policy table's keys.
type Inherited struct {
	Account           RepoSettings                 `json:"account"`
	AccountSources    map[string]configfile.Source `json:"accountSources"`
	Repository        RepoSettings                 `json:"repository"`
	RepositorySources map[string]configfile.Source `json:"repositorySources"`
}

// FieldPolicy is one setting of the policy table, and whether the
// principal may change it on this account.
type FieldPolicy struct {
	configfile.Policy
	Editable bool `json:"editable"`
}

// CreateAccountRequest creates a dashboard account. Spec is an account entry
// of the file in JSON; each secret is {"value": "..."} or, for a webhook
// secret, {"generate": true}. Adopt re-uses a slug an account held before,
// keeping its review history, which is keyed on the slug.
type CreateAccountRequest struct {
	Slug  string          `json:"slug"`
	Spec  json.RawMessage `json:"spec"`
	Adopt bool            `json:"adopt,omitempty"`
}

// UpdateAccountRequest replaces a dashboard account's spec while it is still
// at Revision. A secret may also be {"keep": true}.
type UpdateAccountRequest struct {
	Revision int64           `json:"revision"`
	Spec     json.RawMessage `json:"spec"`
}

// AccountWriteResult is a written account's new revision. Generated holds
// each server-generated secret, keyed "connections[<name>].<key>",
// shown this once and never again.
type AccountWriteResult struct {
	Slug      string            `json:"slug"`
	Revision  int64             `json:"revision"`
	Generated map[string]string `json:"generated,omitempty"`
}

// Accepted is an action queued; JobID is the queued job, when there is
// one.
type Accepted struct {
	JobID int64 `json:"jobId,omitempty"`
}

// AuditAction names what an audit event records.
type AuditAction string

// Audited actions.
const (
	AuditAccountCreate AuditAction = "account.create"
	AuditAccountUpdate AuditAction = "account.update"
	AuditAccountDelete AuditAction = "account.delete"
	AuditAccountAdopt  AuditAction = "account.adopt"
	AuditReviewRerun   AuditAction = "review.rerun"
	AuditReviewCancel  AuditAction = "review.cancel"
	AuditRepoReindex   AuditAction = "repo.reindex"
)

// Valid reports whether a is an audited action.
func (a AuditAction) Valid() bool {
	switch a {
	case AuditAccountCreate, AuditAccountUpdate, AuditAccountDelete, AuditAccountAdopt, AuditReviewRerun, AuditReviewCancel,
		AuditRepoReindex:
		return true
	}
	return false
}

func (a AuditAction) String() string { return string(a) }

// AuditEvent is one audit log entry. Actor is null once the user is
// deleted; Account is the account's slug, "" when the event names none or
// the account is gone. Detail never holds a secret.
type AuditEvent struct {
	ID      string          `json:"id"`
	At      time.Time       `json:"at"`
	Actor   *User           `json:"actor"`
	Account string          `json:"account"`
	Action  AuditAction     `json:"action"`
	Target  string          `json:"target"`
	Detail  json.RawMessage `json:"detail"`
}
