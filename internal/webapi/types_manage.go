package webapi

import (
	"encoding/json"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

// The management API's JSON shapes; internal/web/src/lib/types.ts mirrors
// these too.

// Meta is what the dashboard needs before anyone signs in.
type Meta struct {
	Version string `json:"version"`
	// Management is whether the configuration can be written: a sealing
	// key is configured.
	Management bool   `json:"management"`
	WebURL     string `json:"webUrl"`
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
// Editable is whether the principal may change it: an admin changes every
// setting, and nobody else changes any.
type AccountConfig struct {
	Revision  int64           `json:"revision"`
	Editable  bool            `json:"editable"`
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

// AppInstallation is one account a connection's GitHub App is installed
// on. Served is whether the connection lists the account: kritik reviews
// nothing on one it does not, and an admin may uninstall the App there.
type AppInstallation struct {
	ID          int64  `json:"id"`
	Account     string `json:"account"`
	AccountType string `json:"accountType"`
	// AllRepositories is whether it covers every repository of the
	// account rather than those selected.
	AllRepositories bool   `json:"allRepositories"`
	Suspended       bool   `json:"suspended"`
	Served          bool   `json:"served"`
	URL             string `json:"url,omitempty"`
}

// SetupStatus is how far the instance is from reviewing (ADR-0014 §2.6),
// what the first-run wizard shows and resumes from.
type SetupStatus struct {
	// WebURL is the dashboard's URL, and HooksURL where each connection's
	// webhook goes, its name appended.
	WebURL   string `json:"webUrl"`
	HooksURL string `json:"hooksUrl"`
	// FileConnections are the connections the configuration file and its
	// environment declare, and Connections the running ones.
	FileConnections []string `json:"fileConnections"`
	Connections     []string `json:"connections"`
	// ReviewModel is defaults.models.review, which validation holds to a
	// model an instance provider serves; "" when unset.
	ReviewModel string `json:"reviewModel"`
	// Embedding is whether an embedder is set.
	Embedding bool `json:"embedding"`
}

// ProviderTestRequest tests a model provider's key before it is saved.
// APIKey is {"value": "..."}, or {"keep": true} for the key the running
// provider Name holds, an account's own when Account, "<forge>/<name>",
// names one; a kept key is tested only at its own type and endpoint.
type ProviderTestRequest struct {
	Type    configfile.ProviderType `json:"type"`
	BaseURL string                  `json:"baseUrl,omitempty"`
	APIKey  json.RawMessage         `json:"apiKey"`
	Name    string                  `json:"name,omitempty"`
	Account string                  `json:"account,omitempty"`
}

// EmbeddingTestRequest tests an embedder before it is saved: one input
// embedded at Dims. APIKey is as ProviderTestRequest's, {"keep": true}
// naming the running embedder's key.
type EmbeddingTestRequest struct {
	BaseURL string          `json:"baseUrl"`
	Model   string          `json:"model"`
	Dims    int             `json:"dims"`
	APIKey  json.RawMessage `json:"apiKey"`
}

// TestResult is a test's outcome: the provider's error verbatim when it
// failed, and the models a provider offers when it lists them.
type TestResult struct {
	OK     bool     `json:"ok"`
	Error  string   `json:"error,omitempty"`
	Models []string `json:"models,omitempty"`
}

// AccountRepositories is one account a connection serves: whether its App
// is installed there, and the repositories it reaches.
type AccountRepositories struct {
	Account      string          `json:"account"`
	Installed    bool            `json:"installed"`
	Repositories []AppRepository `json:"repositories"`
}

// AppRepository is one repository an App reaches.
type AppRepository struct {
	Name          string `json:"name"`
	FullName      string `json:"fullName"`
	DefaultBranch string `json:"defaultBranch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
}

// RegisterResult is how many repositories a registration added.
type RegisterResult struct {
	Added int `json:"added"`
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
	AuditAppUninstall  AuditAction = "app.uninstall"
	AuditReviewRerun   AuditAction = "review.rerun"
	AuditReviewCancel  AuditAction = "review.cancel"
	AuditRepoReindex   AuditAction = "repo.reindex"
)

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
