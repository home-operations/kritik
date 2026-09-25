package webapi

import (
	"encoding/json"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
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
// secret is {"set": bool}. Revision is null for a file tenant.
type TenantConfig struct {
	ManagedBy          configfile.Origin `json:"managedBy"`
	Revision           *int64            `json:"revision"`
	Editable           bool              `json:"editable"`
	OperatorOnlyFields []string          `json:"operatorOnlyFields"`
	Spec               json.RawMessage   `json:"spec"`
}

// CreateTenantRequest creates a dashboard tenant. Spec is a tenant entry
// of the file in JSON; each secret is {"value": "..."} or, for a webhook
// secret, {"generate": true}.
type CreateTenantRequest struct {
	Slug string          `json:"slug"`
	Spec json.RawMessage `json:"spec"`
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

// MemberSource is one source of a member's access.
type MemberSource struct {
	Source store.MembershipSource `json:"source"`
	Role   auth.Role              `json:"role"`
}

// Member is an account with access to a tenant; Role is the highest its
// sources give.
type Member struct {
	Account Account        `json:"account"`
	Role    auth.Role      `json:"role"`
	Sources []MemberSource `json:"sources"`
}

// Invite is a pending invitation. CreatedBy is null once that account is
// deleted.
type Invite struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      auth.Role `json:"role"`
	CreatedBy *Account  `json:"createdBy"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Members is a tenant's members; Invites is null unless the principal is
// an admin.
type Members struct {
	Members []Member `json:"members"`
	Invites []Invite `json:"invites"`
}

// CreateInviteRequest invites an email to a tenant. TTLHours defaults to
// a week and may be at most thirty days.
type CreateInviteRequest struct {
	Email    string    `json:"email"`
	Role     auth.Role `json:"role"`
	TTLHours *int      `json:"ttlHours,omitempty"`
}

// UpdateMemberRequest changes a member's invite-granted role.
type UpdateMemberRequest struct {
	Role auth.Role `json:"role"`
}

// MemberRemoved says what removing a member did.
type MemberRemoved struct {
	Note string `json:"note"`
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
	AuditInviteCreate AuditAction = "invite.create"
	AuditInviteDelete AuditAction = "invite.delete"
	AuditMemberUpdate AuditAction = "member.update"
	AuditMemberRemove AuditAction = "member.remove"
	AuditReviewRerun  AuditAction = "review.rerun"
	AuditReviewCancel AuditAction = "review.cancel"
	AuditRepoReindex  AuditAction = "repo.reindex"
)

// Valid reports whether a is an audited action.
func (a AuditAction) Valid() bool {
	switch a {
	case AuditTenantCreate, AuditTenantUpdate, AuditTenantDelete, AuditInviteCreate, AuditInviteDelete,
		AuditMemberUpdate, AuditMemberRemove, AuditReviewRerun, AuditReviewCancel, AuditRepoReindex:
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
