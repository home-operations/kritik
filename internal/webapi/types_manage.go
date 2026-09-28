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
	// Management is whether dashboard tenants can be written: a sealing
	// key is configured.
	Management bool                `json:"management"`
	SignIn     []auth.ProviderInfo `json:"signIn"`
	WebURL     string              `json:"webUrl"`
}

// TenantConfig is a tenant's spec as the principal may see it: every
// secret is {"set": bool}. Revision is null for a file tenant. Policy is
// the policy table as the principal meets it here, for the dashboard to
// render which settings it may change.
type TenantConfig struct {
	ManagedBy configfile.Origin `json:"managedBy"`
	Revision  *int64            `json:"revision"`
	Editable  bool              `json:"editable"`
	Policy    []FieldPolicy     `json:"policy"`
	Inherited Inherited         `json:"inherited"`
	Spec      json.RawMessage   `json:"spec"`
}

// Inherited is what a tenant's settings resolve to where its spec leaves a
// field out: Tenant for the tenant's own fields, from the defaults, and
// Repository for its repository entries, which inherit the tenant's. The
// sources say where each value comes from, by the policy table's keys.
type Inherited struct {
	Tenant            RepoSettings                 `json:"tenant"`
	TenantSources     map[string]configfile.Source `json:"tenantSources"`
	Repository        RepoSettings                 `json:"repository"`
	RepositorySources map[string]configfile.Source `json:"repositorySources"`
}

// FieldPolicy is one setting of the policy table, and whether the
// principal may change it on this tenant.
type FieldPolicy struct {
	configfile.Policy
	Editable bool `json:"editable"`
}

// CreateTenantRequest creates a dashboard tenant. Spec is a tenant entry
// of the file in JSON; each secret is {"value": "..."} or, for a webhook
// secret, {"generate": true}. Adopt re-uses a slug a tenant held before,
// keeping its review history, which is keyed on the slug.
type CreateTenantRequest struct {
	Slug  string          `json:"slug"`
	Spec  json.RawMessage `json:"spec"`
	Adopt bool            `json:"adopt,omitempty"`
}

// UpdateTenantRequest replaces a dashboard tenant's spec while it is still
// at Revision. A secret may also be {"keep": true}.
type UpdateTenantRequest struct {
	Revision int64           `json:"revision"`
	Spec     json.RawMessage `json:"spec"`
}

// TenantWriteResult is a written tenant's new revision. Generated holds
// each server-generated secret, keyed "installations[<name>].<key>",
// shown this once and never again.
type TenantWriteResult struct {
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
	AuditTenantCreate AuditAction = "tenant.create"
	AuditTenantUpdate AuditAction = "tenant.update"
	AuditTenantDelete AuditAction = "tenant.delete"
	AuditTenantAdopt  AuditAction = "tenant.adopt"
	AuditReviewRerun  AuditAction = "review.rerun"
	AuditReviewCancel AuditAction = "review.cancel"
	AuditRepoReindex  AuditAction = "repo.reindex"
)

// Valid reports whether a is an audited action.
func (a AuditAction) Valid() bool {
	switch a {
	case AuditTenantCreate, AuditTenantUpdate, AuditTenantDelete, AuditTenantAdopt, AuditReviewRerun, AuditReviewCancel,
		AuditRepoReindex:
		return true
	}
	return false
}

func (a AuditAction) String() string { return string(a) }

// AuditEvent is one audit log entry. Actor is null once the account is
// deleted; Tenant is the tenant's slug, "" when the event names none or
// the tenant is gone. Detail never holds a secret.
type AuditEvent struct {
	ID     string          `json:"id"`
	At     time.Time       `json:"at"`
	Actor  *Account        `json:"actor"`
	Tenant string          `json:"tenant"`
	Action AuditAction     `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
}
