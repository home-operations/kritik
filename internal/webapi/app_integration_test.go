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
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// fakeGitHub serves the manifest conversion: code "good" registers
// kritik-org-9 under org-9, any other code has expired.
func fakeGitHub(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/app-manifests/good/conversions" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"slug": "kritik-org-9", "client_id": "Iv1.org9", "client_secret": "client-secret", "webhook_secret": "hook-secret",
			"pem": pemKey, "owner": map[string]any{"login": "org-9"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
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

// startManifest starts a flow as who and returns its state.
func (e *manageEnv) startManifest(who string, req AppManifestRequest) (string, AppManifestForm) {
	e.t.Helper()
	status, body := e.do(who, "POST", "/api/v1/app/manifests", req)
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
	status, body := e.do("operator", "GET", instancePath, nil)
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
	e.signIn("operator-2", "mgr-op-2", store.SessionGrant{Role: store.RoleAdmin})

	t.Run("refusals", func(t *testing.T) {
		for _, tt := range []struct {
			name, who string
			req       AppManifestRequest
			status    int
		}{
			{"a member", "outsider", AppManifestRequest{Connection: "mgr-app"}, http.StatusForbidden},
			{"a bad name", "operator", AppManifestRequest{Connection: "Mgr App"}, http.StatusUnprocessableEntity},
			{"the file's connection", "operator", AppManifestRequest{Connection: "mgr-file-bot"}, http.StatusUnprocessableEntity},
			{"a bad organization", "operator", AppManifestRequest{Connection: "mgr-app", Organization: "org/9"}, http.StatusUnprocessableEntity},
		} {
			t.Run(tt.name, func(t *testing.T) {
				status, body := e.do(tt.who, "POST", "/api/v1/app/manifests", tt.req)
				e.expect(status, body, tt.status, "")
			})
		}
	})

	state, form := e.startManifest("operator", AppManifestRequest{Connection: "mgr-app", Organization: "org-9"})
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
	for who, want := range map[string]int{"outsider": http.StatusForbidden, "operator-2": http.StatusBadRequest} {
		if resp := e.visit(who, callback); resp.StatusCode != want {
			t.Fatalf("callback as %s = %d, want %d", who, resp.StatusCode, want)
		}
	}
	resp := e.visit("operator", callback)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#/operator" {
		t.Fatalf("callback = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := e.visit("operator", callback); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a second callback = %d, want 400", resp.StatusCode)
	}

	checkAppConnection(t, e)
	if n := e.audits(AuditAppCreate, "mgr-app"); n != 1 {
		t.Fatalf("app.create audits = %d, want 1", n)
	}
	e.waitFor("the App's connection", func(f *configfile.File) bool { _, ok := f.Connection("mgr-app"); return ok })

	if got := e.collect("operator-2"); len(got) != 0 {
		t.Fatalf("another admin collected %+v", got)
	}
	want := AppManifestResult{
		Connection: "mgr-app", Slug: "kritik-org-9", InstallURL: "https://github.com/apps/kritik-org-9/installations/new",
		ClientID: "Iv1.org9", ClientSecret: "client-secret",
	}
	if got := e.collect("operator"); len(got) != 1 || got[0] != want {
		t.Fatalf("collect = %+v, want %+v", got, want)
	}
	if got := e.collect("operator"); len(got) != 0 {
		t.Fatalf("a result is shown once, got %+v", got)
	}

	t.Run("a name taken meanwhile", func(t *testing.T) {
		if status, body := e.do("operator", "POST", "/api/v1/app/manifests", AppManifestRequest{Connection: "mgr-app"}); status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d: %s", status, body)
		}
	})
	t.Run("an expired code", func(t *testing.T) {
		state, _ := e.startManifest("operator", AppManifestRequest{Connection: "mgr-late"})
		if resp := e.visit("operator", "/app/callback?code=stale&state="+url.QueryEscape(state)); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("callback = %d", resp.StatusCode)
		}
		got := e.collect("operator")
		if len(got) != 1 || got[0].Slug != "" || !strings.Contains(got[0].Error, "GitHub did not return the App's credentials") {
			t.Fatalf("collect = %+v", got)
		}
	})
	if resp := e.visit("operator", "/app/installed?installation_id=1&setup_action=install"); resp.Header.Get("Location") != "/#/operator" {
		t.Fatalf("setup URL redirects to %q", resp.Header.Get("Location"))
	}
}
