package webapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge/github"
	"github.com/home-operations/kritik/internal/store"
)

// The GitHub App manifest flow (ADR-0012, ADR-0014 §2.3): an admin starts
// it in the admin console, GitHub registers the App and sends them back to
// the callback, which adds the App as a dashboard connection, and the
// admin console then shows the App's client secret once.

// githubLoginRe is what GitHub allows in a user or organization login.
var githubLoginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// registerApp mounts the manifest flow's API routes.
func (s *Server) registerApp(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/app/manifests", s.handler(s.createAppManifest))
	mux.HandleFunc("POST /api/v1/app/manifests/collect", s.handler(s.collectAppManifests))
}

// registerAppPages mounts the pages GitHub sends the admin's browser to.
func (s *Server) registerAppPages(mux *http.ServeMux) {
	mux.HandleFunc("GET "+github.ManifestCallbackPath, s.appCallback)
	mux.HandleFunc("GET "+github.ManifestSetupPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.consolePath(), http.StatusSeeOther)
	})
}

// consolePath is the admin console in the dashboard.
func (s *Server) consolePath() string { return s.basePath + "/#/admin" }

// sessionToken is the request's session cookie, which a manifest flow is
// bound to.
func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(auth.SessionCookieName(s.webURL))
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) createAppManifest(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	switch {
	case !p.Admin:
		return errForbidden
	case s.keyring == nil:
		return errManagementDisabled
	}
	var req AppManifestRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if err := s.checkNewConnection(req.Connection); err != nil {
		return err
	}
	if req.Organization != "" && !githubLoginRe.MatchString(req.Organization) {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "organization must be a GitHub organization's login",
			pathDetails{Path: "organization"})
	}
	state, err := s.store.CreateAppManifest(r.Context(), s.sessionToken(r), req.Connection, s.now())
	if err != nil {
		return err
	}
	name := cmp.Or(strings.TrimSpace(req.Name), "kritik-"+req.Connection)
	manifest, err := json.Marshal(github.NewManifest(name, s.webURL.String(), req.Connection, req.Public))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, AppManifestForm{URL: github.RegisterURL(req.Organization, state), Manifest: manifest})
	return nil
}

// checkNewConnection refuses a name no connection may take, or one a
// connection already has, running or left out.
func (s *Server) checkNewConnection(name string) error {
	refuse := func(msg string) error {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, msg, pathDetails{Path: "connection"})
	}
	if !configfile.ValidConnectionName(name) {
		return refuse("a connection name is lowercase alphanumerics and hyphens, 1 to 63 characters")
	}
	f := s.current.Get()
	if _, ok := f.Connection(name); ok {
		return refuse("a connection named " + name + " already exists")
	}
	for _, in := range f.FileConnections() {
		if in.Name == name {
			return refuse("the configuration file declares a connection named " + name)
		}
	}
	return nil
}

// appCallback is where GitHub returns the admin with the code for the App
// it registered. It finishes the flow the state names, recording the App
// or why it failed for the admin console to show, and sends the admin
// there.
func (s *Server) appCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := auth.PrincipalFrom(ctx)
	if p == nil || !p.Admin || s.keyring == nil {
		http.Error(w, "Sign in to kritik as an admin to finish registering the GitHub App.", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	connection, err := s.store.ClaimAppManifest(ctx, state, s.sessionToken(r), s.now())
	if errors.Is(err, store.ErrAppManifest) {
		http.Error(w, "This GitHub App registration was not started in this session, was already finished, or has expired. "+
			"Start it again from the admin console.", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "webapi: claim App manifest", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	res := s.addApp(ctx, p, connection, q.Get("code"))
	if err := s.store.FinishAppManifest(ctx, state, res, s.now()); err != nil {
		s.logger.ErrorContext(ctx, "webapi: finish App manifest", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.consolePath(), http.StatusSeeOther)
}

// addApp converts code to the App's credentials and adds the App to the
// instance spec as connection, serving the account it was registered
// under. The result holds the client secret sealed.
func (s *Server) addApp(ctx context.Context, p *auth.Principal, connection, code string) store.AppManifestResult {
	res := store.AppManifestResult{Connection: connection}
	creds, err := github.ConvertManifest(ctx, s.githubAPI, code)
	if err != nil {
		s.logger.WarnContext(ctx, "webapi: GitHub App manifest not converted", "connection", connection, "error", err)
		res.Error = "GitHub did not return the App's credentials: " + err.Error()
		return res
	}
	res.Slug, res.ClientID = creds.Slug, creds.ClientID
	stored, err := s.store.InstanceSpec(ctx)
	if err == nil {
		_, err = s.writeSpec(ctx, p, stored.Revision, false, "", AuditAppCreate, connection,
			func(spec json.RawMessage) (json.RawMessage, error) { return withConnection(spec, connection, creds) }, specFailure)
	}
	if err != nil {
		s.logger.WarnContext(ctx, "webapi: registered GitHub App not added", "connection", connection, "app", creds.Slug, "error", err)
		res.Error = "GitHub registered the App " + creds.Slug + ", but kritik could not add it as connection " + connection + ": " +
			errorMessage(err) + ". Delete the App on GitHub and start again."
		return res
	}
	if res.ClientSecret, err = s.keyring.Seal([]byte(creds.ClientSecret)); err != nil {
		s.logger.ErrorContext(ctx, "webapi: seal App client secret", "error", err)
		res.ClientSecret = ""
		res.Error = "The App was added, but its client secret could not be kept to show: generate a new one on GitHub to sign in with it."
	}
	return res
}

// withConnection is the stored spec, its secrets kept, with a connection
// for the App creds names added.
func withConnection(stored json.RawMessage, name string, creds github.AppCredentials) (json.RawMessage, error) {
	root, err := decodeObject(keepSecrets(stored))
	if err != nil {
		return nil, err
	}
	conns, _ := root["connections"].([]any)
	root["connections"] = append(conns, map[string]any{
		nameKey: name, "forge": string(configfile.ForgeGitHub), "accounts": []any{creds.Owner},
		"app": map[string]any{
			"clientId":      creds.ClientID,
			"privateKey":    map[string]any{valueKey: creds.PEM},
			"webhookSecret": map[string]any{valueKey: creds.WebhookSecret},
		},
	})
	return json.Marshal(root)
}

// errorMessage is err's message as the API would show it.
func errorMessage(err error) string {
	if ae, ok := errors.AsType[*apiError](err); ok {
		return ae.message
	}
	return err.Error()
}

func (s *Server) collectAppManifests(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	switch {
	case !p.Admin:
		return errForbidden
	case s.keyring == nil:
		return errManagementDisabled
	}
	results, err := s.store.CollectAppManifests(r.Context(), s.sessionToken(r))
	if err != nil {
		return err
	}
	out := make([]AppManifestResult, 0, len(results))
	for _, x := range results {
		o := AppManifestResult{Connection: x.Connection, Slug: x.Slug, ClientID: x.ClientID, Error: x.Error}
		if x.Slug != "" {
			o.InstallURL = "https://github.com/apps/" + url.PathEscape(x.Slug) + "/installations/new"
		}
		if x.ClientSecret != "" {
			plain, err := s.keyring.Open(x.ClientSecret)
			if err != nil {
				return err
			}
			o.ClientSecret = string(plain)
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
