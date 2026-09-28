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
	// Management is whether the configuration can be written: a sealing
	// key is configured.
	Management bool                `json:"management"`
	SignIn     []auth.ProviderInfo `json:"signIn"`
	WebURL     string              `json:"webUrl"`
}

// InstanceConfig is the instance spec as an admin sees it: every secret is
// {"set": bool}. Revision is the stored spec's, 0 before any write.
type InstanceConfig struct {
	Revision int64           `json:"revision"`
	Editable bool            `json:"editable"`
	Spec     json.RawMessage `json:"spec"`
}

// AccountConfig is an account's entry in the instance spec, every secret
// {"set": bool}; an account the spec lists no entry for gets one naming it.
// Revision is the instance spec's, which a write of the entry must match.
// Policy is the policy table as the principal meets it here, for the
// dashboard to render which settings it may change.
type AccountConfig struct {
	Revision  int64           `json:"revision"`
	Editable  bool            `json:"editable"`
	Policy    []FieldPolicy   `json:"policy"`
	Inherited Inherited       `json:"inherited"`
	Spec      json.RawMessage `json:"spec"`
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

// UpdateConfigRequest replaces the instance spec, or one account's entry
// in it, while the instance spec is still at Revision, 0 when none is
// stored. Each secret is {"value": "..."}, {"keep": true} or, for a
// webhook secret, {"generate": true}. ConfirmReindex accepts that the
// write's new embedder model or dimension rebuilds every repository's
// index; without it such a write is refused with CodeReindexRequired.
type UpdateConfigRequest struct {
	Revision       int64           `json:"revision"`
	Spec           json.RawMessage `json:"spec"`
	ConfirmReindex bool            `json:"confirmReindex,omitempty"`
}

// ConfigWriteResult is the instance spec's new revision. Generated holds
// each server-generated secret, keyed "connections[<name>].<key>", shown
// this once and never again.
type ConfigWriteResult struct {
	Revision  int64             `json:"revision"`
	Generated map[string]string `json:"generated,omitempty"`
}

// AppManifestRequest starts registering a GitHub App from a manifest for
// a new dashboard connection (ADR-0014 §2.3).
type AppManifestRequest struct {
	// Connection names the connection the App will serve, and so its hook
	// path.
	Connection string `json:"connection"`
	// Organization registers the App under that organization, "" under
	// the admin's own GitHub account.
	Organization string `json:"organization,omitempty"`
	// Name is the App's name on GitHub, "kritik-<connection>" when empty;
	// the admin may still change it there.
	Name string `json:"name,omitempty"`
	// Public lets any account install the App; only the accounts its
	// connection lists are served.
	Public bool `json:"public"`
}

// AppManifestForm is what the browser POSTs to GitHub: Manifest, as the
// form's manifest field, to URL, which carries the flow's state.
type AppManifestForm struct {
	URL      string          `json:"url"`
	Manifest json.RawMessage `json:"manifest"`
}

// AppManifestResult is one finished manifest flow, read once: the App
// registered for Connection and where to install it, with its client ID
// and secret for signing in with GitHub through it, which kritik does not
// keep; or why the flow failed.
type AppManifestResult struct {
	Connection   string `json:"connection"`
	Slug         string `json:"slug,omitempty"`
	InstallURL   string `json:"installUrl,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Error        string `json:"error,omitempty"`
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
	AuditConfigUpdate  AuditAction = "config.update"
	AuditAccountUpdate AuditAction = "account.update"
	AuditAppCreate     AuditAction = "app.create"
	AuditReviewRerun   AuditAction = "review.rerun"
	AuditReviewCancel  AuditAction = "review.cancel"
	AuditRepoReindex   AuditAction = "repo.reindex"
)

// Valid reports whether a is an audited action.
func (a AuditAction) Valid() bool {
	switch a {
	case AuditConfigUpdate, AuditAccountUpdate, AuditAppCreate, AuditReviewRerun, AuditReviewCancel, AuditRepoReindex:
		return true
	}
	return false
}

func (a AuditAction) String() string { return string(a) }

// AuditEvent is one audit log entry. Actor is null once the user is
// deleted; Account is the account's slug, "" when the event names none or
// no connection serves the account now. Detail never holds a secret.
type AuditEvent struct {
	ID      string          `json:"id"`
	At      time.Time       `json:"at"`
	Actor   *User           `json:"actor"`
	Account string          `json:"account"`
	Action  AuditAction     `json:"action"`
	Target  string          `json:"target"`
	Detail  json.RawMessage `json:"detail"`
}
