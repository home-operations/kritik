package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Manifest is a GitHub App manifest (ADR-0012, ADR-0014 §2.3): the App
// kritik registers for a connection, with its webhook, permissions and
// events already set.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     HookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	CallbackURLs       []string          `json:"callback_urls"`
	SetupURL           string            `json:"setup_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// HookAttributes is a manifest's webhook.
type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// AppPermissions and AppEvents are what kritik needs of its App, as the
// setup guide lists them. members is an organization permission: with it,
// the same App signs people in and reads which organizations they belong
// to (ADR-0014 §2.5).
var (
	AppPermissions = map[string]string{
		"contents":      permRead,
		"metadata":      permRead,
		"pull_requests": permWrite,
		"issues":        permRead,
		"statuses":      permWrite,
		"members":       permRead,
	}
	AppEvents = []string{"pull_request", "pull_request_review_comment", "issue_comment", "push"}
)

// Permission levels a manifest grants.
const (
	permRead  = "read"
	permWrite = "write"
)

// Manifest paths under the dashboard's URL: where GitHub returns the admin
// with the code, and where it sends them after they install the App.
const (
	ManifestCallbackPath = "/app/callback"
	ManifestSetupPath    = "/app/installed"
)

// NewManifest is the manifest of an App named name that serves connection
// for the dashboard at webURL, whose webhook listener is under the same
// URL. A public App can be installed by any account.
func NewManifest(name, webURL, connection string, public bool) Manifest {
	base := strings.TrimSuffix(webURL, "/")
	return Manifest{
		Name:               name,
		URL:                base,
		HookAttributes:     HookAttributes{URL: base + "/hooks/" + url.PathEscape(connection), Active: true},
		RedirectURL:        base + ManifestCallbackPath,
		CallbackURLs:       []string{base + "/auth/callback/github"},
		SetupURL:           base + ManifestSetupPath,
		Public:             public,
		DefaultPermissions: AppPermissions,
		DefaultEvents:      AppEvents,
	}
}

// RegisterURL is where a form POSTs a manifest, in its manifest field: the
// signed-in person's own settings when org is "", else the organization's.
// state comes back with GitHub's redirect.
func RegisterURL(org, state string) string {
	u := "https://github.com/settings/apps/new"
	if org != "" {
		u = "https://github.com/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	return u + "?state=" + url.QueryEscape(state)
}

// AppCredentials are what GitHub returns for an App it registered from a
// manifest.
type AppCredentials struct {
	Slug, ClientID, ClientSecret, WebhookSecret, PEM string
	// Owner is the login of the account the App was registered under.
	Owner string
}

// ConvertManifest trades the code GitHub's redirect carries for the App's
// credentials. The endpoint takes no credentials. apiBase is empty for
// api.github.com; tests point it at a server of their own.
func ConvertManifest(ctx context.Context, apiBase, code string) (AppCredentials, error) {
	client, err := newClient(http.DefaultTransport, apiBase)
	if err != nil {
		return AppCredentials{}, err
	}
	cfg, _, err := client.Apps.CompleteAppManifest(ctx, code)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("github: convert App manifest: %w", err)
	}
	creds := AppCredentials{
		Slug: cfg.GetSlug(), ClientID: cfg.GetClientID(), ClientSecret: cfg.GetClientSecret(),
		WebhookSecret: cfg.GetWebhookSecret(), PEM: cfg.GetPEM(), Owner: cfg.GetOwner().GetLogin(),
	}
	if creds.Slug == "" || creds.ClientID == "" || creds.PEM == "" || creds.WebhookSecret == "" || creds.Owner == "" {
		return AppCredentials{}, fmt.Errorf("github: convert App manifest: GitHub returned incomplete credentials for %q", creds.Slug)
	}
	return creds, nil
}
