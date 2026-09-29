//go:build integration

package webapi

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// fakeGitHubServer is a GitHub API for the App kritik-org-9.
type fakeGitHubServer struct {
	url     string
	mu      sync.Mutex
	deleted []string
}

// fakeGitHub serves the manifest conversion, where code "good" registers
// kritik-org-9 under org-9 and any other code has expired, and that App's
// installations: on org-9 and on stranger.
func fakeGitHub(t *testing.T) *fakeGitHubServer {
	t.Helper()
	f := &fakeGitHubServer{}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/app-manifests/good/conversions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"slug": "kritik-org-9", "client_id": "Iv1.org9", "client_secret": "client-secret", "webhook_secret": "hook-secret",
				"pem": pemKey, "owner": map[string]any{"login": "org-9"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/app/installations":
			_, _ = w.Write([]byte(`[{"id":1,"account":{"login":"org-9","type":"Organization"},"repository_selection":"all"},` +
				`{"id":2,"account":{"login":"stranger","type":"User"},"repository_selection":"selected"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/app/installations/1/access_tokens":
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			_, _ = w.Write([]byte(`{"token":"ghs_1","expires_at":"` + exp + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/installation/repositories":
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"name":"repo-1","full_name":"org-9/repo-1","default_branch":"main"}]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/app/installations/"):
			f.mu.Lock()
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/api/v3/app/installations/"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// visit sends a browser navigation as who, without following redirects.
func (e *manageEnv) visit(who, path string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if c := e.cookie[who]; c != nil {
		req.AddCookie(c)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// startManifest starts a flow as the admin and returns its state.
func (e *manageEnv) startManifest(req AppManifestRequest) (string, AppManifestForm) {
	e.t.Helper()
	status, body := e.do("admin", "POST", "/api/v1/app/manifests", req)
	e.expect(status, body, http.StatusOK, "")
	var form AppManifestForm
	if err := json.Unmarshal(body, &form); err != nil {
		e.t.Fatal(err)
	}
	u, err := url.Parse(form.URL)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.Query().Get("state"), form
}

func (e *manageEnv) collect(who string) []AppManifestResult {
	e.t.Helper()
	status, body := e.do(who, "POST", "/api/v1/app/manifests/collect", nil)
	e.expect(status, body, http.StatusOK, "")
	var out []AppManifestResult
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// checkAppConnection checks the spec holds the connection the fake
// GitHub's App was added as, its secrets stored.
func checkAppConnection(t *testing.T, e *manageEnv) {
	t.Helper()
	status, body := e.do("admin", "GET", instancePath, nil)
	e.expect(status, body, http.StatusOK, "")
	var cfg struct{ Spec map[string]any }
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	conns, _ := cfg.Spec["connections"].([]any)
	if len(conns) != 1 {
		t.Fatalf("connections = %v", cfg.Spec["connections"])
	}
	conn := conns[0].(map[string]any)
	app := conn["app"].(map[string]any)
	if conn["name"] != "mgr-app" || conn["accounts"].([]any)[0] != "org-9" || app["clientId"] != "Iv1.org9" ||
		app["privateKey"].(map[string]any)["set"] != true || app["webhookSecret"].(map[string]any)["set"] != true {
		t.Fatalf("connection = %v", conn)
	}
}

// TestAppManifestFlow walks a GitHub App registration from the admin
// console to the connection it adds and the client secret shown once.
func TestAppManifestFlow(t *testing.T) {
	e := newManageEnv(t)
	e.signIn("admin-2", "mgr-op-2", store.SessionGrant{Role: store.RoleAdmin})

	t.Run("refusals", func(t *testing.T) {
		for _, tt := range []struct {
			name, who string
			req       AppManifestRequest
			status    int
		}{
			{"a member", "outsider", AppManifestRequest{Connection: "mgr-app"}, http.StatusForbidden},
			{"a bad name", "admin", AppManifestRequest{Connection: "Mgr App"}, http.StatusUnprocessableEntity},
			{"the file's connection", "admin", AppManifestRequest{Connection: "mgr-file-bot"}, http.StatusUnprocessableEntity},
			{"a bad organization", "admin", AppManifestRequest{Connection: "mgr-app", Organization: "org/9"}, http.StatusUnprocessableEntity},
		} {
			t.Run(tt.name, func(t *testing.T) {
				status, body := e.do(tt.who, "POST", "/api/v1/app/manifests", tt.req)
				e.expect(status, body, tt.status, "")
			})
		}
	})

	state, form := e.startManifest(AppManifestRequest{Connection: "mgr-app", Organization: "org-9"})
	if !strings.HasPrefix(form.URL, "https://github.com/organizations/org-9/settings/apps/new?state=") || state == "" {
		t.Fatalf("form URL = %s", form.URL)
	}
	var manifest map[string]any
	if err := json.Unmarshal(form.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["name"] != "kritik-mgr-app" || manifest["public"] != false ||
		manifest["hook_attributes"].(map[string]any)["url"] != "https://kritik.example/hooks/mgr-app" {
		t.Fatalf("manifest = %v", manifest)
	}

	callback := "/app/callback?code=good&state=" + url.QueryEscape(state)
	for who, want := range map[string]int{"outsider": http.StatusForbidden, "admin-2": http.StatusBadRequest} {
		if resp := e.visit(who, callback); resp.StatusCode != want {
			t.Fatalf("callback as %s = %d, want %d", who, resp.StatusCode, want)
		}
	}
	resp := e.visit("admin", callback)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#/admin" {
		t.Fatalf("callback = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := e.visit("admin", callback); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a second callback = %d, want 400", resp.StatusCode)
	}

	checkAppConnection(t, e)
	if n := e.audits(AuditAppCreate, "mgr-app"); n != 1 {
		t.Fatalf("app.create audits = %d, want 1", n)
	}
	e.waitFor("the App's connection", func(f *configfile.File) bool { _, ok := f.Connection("mgr-app"); return ok })

	if got := e.collect("admin-2"); len(got) != 0 {
		t.Fatalf("another admin collected %+v", got)
	}
	want := AppManifestResult{
		Connection: "mgr-app", Slug: "kritik-org-9", InstallURL: "https://github.com/apps/kritik-org-9/installations/new",
		ClientID: "Iv1.org9", ClientSecret: "client-secret",
	}
	if got := e.collect("admin"); len(got) != 1 || got[0] != want {
		t.Fatalf("collect = %+v, want %+v", got, want)
	}
	if got := e.collect("admin"); len(got) != 0 {
		t.Fatalf("a result is shown once, got %+v", got)
	}

	t.Run("a name taken meanwhile", func(t *testing.T) {
		if status, body := e.do("admin", "POST", "/api/v1/app/manifests", AppManifestRequest{Connection: "mgr-app"}); status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d: %s", status, body)
		}
	})
	t.Run("an expired code", func(t *testing.T) {
		state, _ := e.startManifest(AppManifestRequest{Connection: "mgr-late"})
		if resp := e.visit("admin", "/app/callback?code=stale&state="+url.QueryEscape(state)); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("callback = %d", resp.StatusCode)
		}
		got := e.collect("admin")
		if len(got) != 1 || got[0].Slug != "" || !strings.Contains(got[0].Error, "GitHub did not return the App's credentials") {
			t.Fatalf("collect = %+v", got)
		}
	})
	if resp := e.visit("admin", "/app/installed?installation_id=1&setup_action=install"); resp.Header.Get("Location") != "/#/admin" {
		t.Fatalf("setup URL redirects to %q", resp.Header.Get("Location"))
	}
}

// TestAppInstallations: the admin console lists every account a
// connection's App is installed on, and removes it from one the
// connection does not serve.
func TestAppInstallations(t *testing.T) {
	e := newManageEnv(t)
	state, _ := e.startManifest(AppManifestRequest{Connection: "mgr-app", Organization: "org-9"})
	if resp := e.visit("admin", "/app/callback?code=good&state="+url.QueryEscape(state)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback = %d", resp.StatusCode)
	}
	e.collect("admin")
	e.waitFor("the App's connection", func(f *configfile.File) bool { _, ok := f.Connection("mgr-app"); return ok })

	status, body := e.do("admin", "GET", "/api/v1/admin/connections", nil)
	e.expect(status, body, http.StatusOK, "")
	var conns []Connection
	if err := json.Unmarshal(body, &conns); err != nil {
		t.Fatal(err)
	}
	origins := map[string]configfile.Origin{}
	for _, c := range conns {
		origins[c.Name] = c.ManagedBy
	}
	if origins["mgr-file-bot"] != configfile.OriginFile || origins["mgr-app"] != configfile.OriginDashboard {
		t.Fatalf("connections = %+v", conns)
	}

	path := "/api/v1/admin/connections/mgr-app/installations"
	status, body = e.do("admin", "GET", path, nil)
	e.expect(status, body, http.StatusOK, "")
	var insts []AppInstallation
	if err := json.Unmarshal(body, &insts); err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 || insts[0].Account != "org-9" || !insts[0].Served || !insts[0].AllRepositories ||
		insts[1].Account != "stranger" || insts[1].Served {
		t.Fatalf("installations = %+v", insts)
	}

	for _, tt := range []struct {
		name, who, path string
		status          int
		code            ErrorCode
	}{
		{"a member", "outsider", path, http.StatusNotFound, ""},
		{"an unknown connection", "admin", "/api/v1/admin/connections/nope/installations", http.StatusNotFound, CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, body := e.do(tt.who, "GET", tt.path, nil)
			e.expect(status, body, tt.status, tt.code)
		})
	}
	for _, tt := range []struct {
		name, id string
		status   int
		code     ErrorCode
	}{
		{"a served account", "1", http.StatusConflict, CodeInstallationServed},
		{"an installation of another App", "3", http.StatusNotFound, CodeNotFound},
		{"an unserved account", "2", http.StatusNoContent, ""},
	} {
		t.Run("uninstall "+tt.name, func(t *testing.T) {
			status, body := e.do("admin", "DELETE", path+"/"+tt.id, nil)
			e.expect(status, body, tt.status, tt.code)
		})
	}
	e.github.mu.Lock()
	deleted := slices.Clone(e.github.deleted)
	e.github.mu.Unlock()
	if !slices.Equal(deleted, []string{"2"}) {
		t.Fatalf("uninstalled %v, want [2]", deleted)
	}
	if n := e.audits(AuditAppUninstall, "mgr-app/stranger"); n != 1 {
		t.Fatalf("app.uninstall audits = %d, want 1", n)
	}
}

// TestReachedRepositories: the wizard lists what each served account's
// installation reaches, and registers it so polling and onboarding know it.
func TestReachedRepositories(t *testing.T) {
	e := newManageEnv(t)
	state, _ := e.startManifest(AppManifestRequest{Connection: "mgr-app", Organization: "org-9"})
	e.visit("admin", "/app/callback?code=good&state="+url.QueryEscape(state))
	e.collect("admin")
	e.waitFor("the App's connection", func(f *configfile.File) bool { _, ok := f.Connection("mgr-app"); return ok })
	// The leader would apply the new account; this suite has none.
	if err := e.st.ApplyConfig(t.Context(), e.src.Current.Get()); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	path := "/api/v1/admin/connections/mgr-app/repositories"
	status, body := e.do("admin", "GET", path, nil)
	e.expect(status, body, http.StatusOK, "")
	var reached []AccountRepositories
	if err := json.Unmarshal(body, &reached); err != nil {
		t.Fatal(err)
	}
	if len(reached) != 1 || reached[0].Account != "org-9" || !reached[0].Installed || len(reached[0].Repositories) != 1 ||
		reached[0].Repositories[0].FullName != "org-9/repo-1" {
		t.Fatalf("reached = %+v", reached)
	}
	for i, want := range []int{1, 0} {
		status, body := e.do("admin", "POST", path, nil)
		e.expect(status, body, http.StatusOK, "")
		var res RegisterResult
		if err := json.Unmarshal(body, &res); err != nil || res.Added != want {
			t.Fatalf("register %d = %s, want %d added", i, body, want)
		}
	}
	if branch := e.scalar(`SELECT default_branch FROM repositories WHERE name = 'org-9/repo-1'`); branch != "main" {
		t.Fatalf("default branch = %q", branch)
	}
	if status, body := e.do("outsider", "GET", path, nil); status != http.StatusNotFound {
		t.Fatalf("a member's listing = %d: %s", status, body)
	}
}
