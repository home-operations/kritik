package webapi

import (
	"encoding/json"
	"time"
)

// The management API's JSON shapes; internal/web/src/lib/types.ts mirrors
// these too.

// Meta is what the dashboard needs before anyone signs in.
type Meta struct {
	Version string `json:"version"`
	WebURL  string `json:"webUrl"`
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

// SetupStatus is how far the instance is from reviewing, what the
// Configuration page's checklist shows (ADR-0019 §2.2).
type SetupStatus struct {
	// WebURL is the dashboard's URL, and HooksURL where each connection's
	// webhook goes, its name appended.
	WebURL   string `json:"webUrl"`
	HooksURL string `json:"hooksUrl"`
	// Connections are the running connections.
	Connections []string `json:"connections"`
	// ReviewModel is defaults.models.review, which validation holds to a
	// model an instance provider serves; "" when unset.
	ReviewModel string `json:"reviewModel"`
	// Embedding is whether an embedder is set.
	Embedding bool `json:"embedding"`
	// ConfigError is why the configuration file's latest content was
	// refused, "" while the running configuration is its latest content.
	ConfigError string `json:"configError"`
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
	AuditAppUninstall AuditAction = "app.uninstall"
	AuditReviewRerun  AuditAction = "review.rerun"
	AuditReviewCancel AuditAction = "review.cancel"
	AuditRepoReindex  AuditAction = "repo.reindex"
	AuditRepoTurnOn   AuditAction = "repo.turn_on"
	AuditRepoTurnOff  AuditAction = "repo.turn_off"
)

// TurnOnRequest turns a repository on, or off.
type TurnOnRequest struct {
	On bool `json:"on"`
}

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
