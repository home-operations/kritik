package webapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/forge/github"
)

const testConfig = `
connections:
  - name: alpha-bot
    forge: github
    accounts: [alpha]
    app: { clientId: Iv1.alpha, privateKey: { env: KRITIK_TEST_TOKEN }, webhookSecret: { env: KRITIK_TEST_TOKEN } }
  - name: beta-bot
    forge: github
    accounts: [beta]
    app: { clientId: Iv1.beta, privateKey: { env: KRITIK_TEST_TOKEN }, webhookSecret: { env: KRITIK_TEST_TOKEN } }
`

func testFile(t *testing.T) *configfile.File {
	t.Helper()
	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	return configfiletest.Load(t, testConfig)
}

func accountIDOf(t *testing.T, f *configfile.File, name string) string {
	t.Helper()
	a, ok := f.Account(configfile.ForgeGitHub, name)
	if !ok {
		t.Fatalf("no account %s", name)
	}
	return a.ID()
}

type testServer struct {
	srv  *Server
	file *configfile.File
	h    http.Handler
}

func newTestServer(t *testing.T, webURL string) *testServer {
	t.Helper()
	f := testFile(t)
	cur := configfile.NewCurrent(f)
	u, err := url.Parse(webURL)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{Current: cur, WebURL: u})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	ui := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>kritik</title>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"favicon.svg":      {Data: []byte("<svg/>")},
		"assets/app-1.css": {Data: []byte("body{}")},
	}
	srv := New(Config{Current: cur, Auth: a, UI: ui, WebURL: u, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return &testServer{srv: srv, file: f, h: srv.Handler()}
}

// as serves r acting as p, or unauthenticated when p is nil.
func (ts *testServer) as(p *auth.Principal, r *http.Request) *httptest.ResponseRecorder {
	if p != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	}
	w := httptest.NewRecorder()
	ts.h.ServeHTTP(w, r)
	return w
}

// memberOf is a member who reads the account name.
func memberOf(t *testing.T, f *configfile.File, name string) *auth.Principal {
	t.Helper()
	return &auth.Principal{
		User:     auth.User{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com"},
		Accounts: map[string]bool{accountIDOf(t, f, name): true},
	}
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", w.Body.String(), err)
	}
	return e
}

func TestAPIRequiresPrincipal(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	for _, path := range []string{"/api/v1/me", "/api/v1/accounts", "/api/v1/accounts/github/alpha/repos", "/api/events", "/api/nope"} {
		t.Run(path, func(t *testing.T) {
			w := ts.as(nil, httptest.NewRequest("GET", path, nil))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestAccountScopeHidesUnreadableAccounts(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	alphaMember := memberOf(t, ts.file, "alpha")
	paths := []string{
		"/api/v1/accounts/%s", "/api/v1/accounts/%s/repos", "/api/v1/accounts/%s/repos/o/r", "/api/v1/accounts/%s/pulls",
		"/api/v1/accounts/%s/pulls/o/r/1", "/api/v1/accounts/%s/reviews/x", "/api/v1/accounts/%s/reviews/x/diff",
		"/api/v1/accounts/%s/reviews/x/transcript", "/api/v1/accounts/%s/reviews/x/raw", "/api/v1/accounts/%s/index-runs",
		"/api/v1/accounts/%s/followups", "/api/v1/accounts/%s/followups/1/transcript", "/api/v1/accounts/%s/usage",
		"/api/v1/accounts/%s/queue",
	}
	for _, slug := range []string{"beta", "nope"} {
		for _, p := range paths {
			path := strings.Replace(p, "%s", slug, 1)
			t.Run(path, func(t *testing.T) {
				w := ts.as(alphaMember, httptest.NewRequest("GET", path, nil))
				if w.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404", w.Code)
				}
				if e := decodeError(t, w); e.Code != CodeNotFound || e.Message == "" {
					t.Errorf("error = %+v, want not_found with a message", e)
				}
			})
		}
	}
}

func TestResolveAccount(t *testing.T) {
	f := testFile(t)
	alpha := accountIDOf(t, f, "alpha")
	tests := []struct {
		name    string
		p       *auth.Principal
		slug    string
		wantErr bool
	}{
		{"member reads own account", &auth.Principal{Accounts: map[string]bool{alpha: true}}, "alpha", false},
		{"member of every account", &auth.Principal{AllAccounts: true}, "beta", false},
		{"member of another account", &auth.Principal{Accounts: map[string]bool{alpha: true}}, "beta", true},
		{"unknown account", &auth.Principal{Admin: true}, "gamma", true},
		{"an admin reads any account", &auth.Principal{Admin: true}, "beta", false},
		{"no memberships", &auth.Principal{}, "alpha", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, err := resolveAccount(f, tt.p, "github", tt.slug)
			if tt.wantErr {
				e, ok := err.(*apiError)
				if !ok || e.status != http.StatusNotFound {
					t.Fatalf("err = %v, want a 404", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAccount: %v", err)
			}
			if sc.account.Name != tt.slug {
				t.Errorf("scope = %s, want %s", sc.account.Name, tt.slug)
			}
		})
	}
}

func TestMe(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	tests := []struct {
		name     string
		p        *auth.Principal
		admin    bool
		accounts []string
	}{
		{"member", memberOf(t, ts.file, "beta"), false, []string{"github/beta"}},
		{"an admin reads every account", &auth.Principal{Admin: true}, true, []string{"github/alpha", "github/beta"}},
		{"no accounts", &auth.Principal{}, false, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.as(tt.p, httptest.NewRequest("GET", "/api/v1/me", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			var me Me
			if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
				t.Fatal(err)
			}
			if me.Admin != tt.admin || !slices.Equal(me.Accounts, tt.accounts) {
				t.Fatalf("me = %+v, want admin %v accounts %v", me, tt.admin, tt.accounts)
			}
		})
	}
}

func TestListAccountsWithNoneReadable(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(&auth.Principal{}, httptest.NewRequest("GET", "/api/v1/accounts", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("got %d %q, want 200 []", w.Code, w.Body)
	}
}

func TestAdminRouteHiddenFromNonAdmins(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(memberOf(t, ts.file, "alpha"), httptest.NewRequest("GET", "/api/v1/admin/accounts", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRequestValidation(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	p := memberOf(t, ts.file, "alpha")
	tests := []struct {
		path string
		code ErrorCode
	}{
		{"/api/v1/accounts/github/alpha/repos?limit=0", CodeBadRequest},
		{"/api/v1/accounts/github/alpha/repos?cursor=@@", CodeInvalidCursor},
		{"/api/v1/accounts/github/alpha/repos?type=mirrors", CodeBadRequest},
		{"/api/v1/accounts/github/alpha/pulls?state=merged", CodeBadRequest},
		{"/api/v1/accounts/github/alpha/pulls?outcome=great", CodeBadRequest},
		{"/api/v1/accounts/github/alpha/usage?group=week", CodeBadRequest},
		{"/api/v1/accounts/github/alpha/usage?from=yesterday", CodeBadRequest},
		{"/api/v1/accounts/github/alpha/usage?from=2026-02-01&to=2026-01-01", CodeBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := ts.as(p, httptest.NewRequest("GET", tt.path, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
			}
			if e := decodeError(t, w); e.Code != tt.code {
				t.Errorf("code = %q, want %q", e.Code, tt.code)
			}
		})
	}
}

func TestUnknownRoutes(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	p := &auth.Principal{Admin: true}
	for _, path := range []string{"/api/v1/nope", "/api/v2/accounts", "/auth/nope"} {
		t.Run(path, func(t *testing.T) {
			w := ts.as(p, httptest.NewRequest("GET", path, nil))
			if w.Code != http.StatusNotFound || decodeError(t, w).Code != CodeNotFound {
				t.Fatalf("got %d %s, want a JSON 404", w.Code, w.Body)
			}
		})
	}
}

func TestMutationsNeedSameOrigin(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(&auth.Principal{Admin: true}, httptest.NewRequest("POST", "/api/v1/me", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the same-origin check", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	for _, path := range []string{"/", "/api/v1/me", "/auth/providers"} {
		t.Run(path, func(t *testing.T) {
			w := ts.as(nil, httptest.NewRequest("GET", path, nil))
			h := w.Header()
			if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Errorf("CSP = %q", got)
			}
			if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
				t.Errorf("headers = %v", h)
			}
		})
	}
	if !strings.Contains(contentSecurityPolicy, "img-src 'self' data: https:;") ||
		!strings.Contains(contentSecurityPolicy, "form-action 'self'") {
		t.Errorf("CSP %q does not match the dashboard's policy", contentSecurityPolicy)
	}
	// The App manifest form posts to GitHub; a form-action that does not
	// admit its target leaves the browser on the dashboard.
	var formAction []string
	for d := range strings.SplitSeq(contentSecurityPolicy, ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == "form-action" {
			formAction = f[1:]
		}
	}
	for _, org := range []string{"", "org-1"} {
		u, err := url.Parse(github.RegisterURL(org, "s1"))
		if err != nil {
			t.Fatal(err)
		}
		if origin := u.Scheme + "://" + u.Host; !slices.Contains(formAction, origin) {
			t.Errorf("form-action %v does not admit %s, where the manifest form posts", formAction, u)
		}
	}
}

func TestUICaching(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	tests := []struct {
		path, cache string
		status      int
	}{
		{"/", "no-cache", http.StatusOK},
		{"/favicon.svg", "no-cache", http.StatusOK},
		{"/assets/app-1.js", "public, max-age=31536000, immutable", http.StatusOK},
		{"/auth/providers", "no-store", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := ts.as(nil, httptest.NewRequest("GET", tt.path, nil))
			if w.Code != tt.status || w.Header().Get("Cache-Control") != tt.cache {
				t.Errorf("got %d Cache-Control %q, want %d %q", w.Code, w.Header().Get("Cache-Control"), tt.status, tt.cache)
			}
		})
	}
	if w := ts.as(nil, httptest.NewRequest("POST", "/", nil)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", w.Code)
	}
}

func TestUIServesFilesOnly(t *testing.T) {
	for _, web := range []string{"https://kritik.example", "https://example.com/kritik/"} {
		ts := newTestServer(t, web)
		base := strings.TrimSuffix(ts.srv.basePath, "/")
		tests := []struct {
			name, path string
			status     int
		}{
			{"the root serves index.html", base + "/", http.StatusOK},
			{"a file", base + "/assets/app-1.js", http.StatusOK},
			{"a directory is not listed", base + "/assets/", http.StatusNotFound},
			{"nor redirected to its listing", base + "/assets", http.StatusNotFound},
		}
		for _, tt := range tests {
			t.Run(web+" "+tt.name, func(t *testing.T) {
				if w := ts.as(nil, httptest.NewRequest("GET", tt.path, nil)); w.Code != tt.status {
					t.Errorf("GET %s = %d, want %d: %s", tt.path, w.Code, tt.status, w.Body)
				}
			})
		}
		// The test server's auth has no store: resolving the cookie would fail
		// the request, so an asset served with one never looked it up.
		t.Run(web+" an asset skips the session lookup", func(t *testing.T) {
			r := httptest.NewRequest("GET", base+"/assets/app-1.css", nil)
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName(ts.srv.webURL), Value: "tok"})
			if w := ts.as(nil, r); w.Code != http.StatusOK {
				t.Errorf("asset with a session cookie = %d, want 200", w.Code)
			}
		})
	}
}

func TestBasePath(t *testing.T) {
	ts := newTestServer(t, "https://example.com/kritik/")
	p := &auth.Principal{Admin: true}
	tests := []struct {
		name, path string
		status     int
		location   string
	}{
		{"bare prefix redirects", "/kritik", http.StatusMovedPermanently, "/kritik/"},
		{"bare prefix keeps the query", "/kritik?x=1", http.StatusMovedPermanently, "/kritik/?x=1"},
		{"ui under prefix", "/kritik/", http.StatusOK, ""},
		{"asset under prefix", "/kritik/assets/app-1.js", http.StatusOK, ""},
		{"api under prefix", "/kritik/api/v1/me", http.StatusOK, ""},
		{"api outside prefix", "/api/v1/me", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.as(p, httptest.NewRequest("GET", tt.path, nil))
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if tt.location != "" && w.Header().Get("Location") != tt.location {
				t.Errorf("Location = %q, want %q", w.Header().Get("Location"), tt.location)
			}
		})
	}
}

func TestRecovererAnswers500(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	h := ts.srv.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusInternalServerError || decodeError(t, w).Code != CodeInternal {
		t.Fatalf("got %d %s, want a JSON 500", w.Code, w.Body)
	}
}
