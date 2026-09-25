package webapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
)

const testConfig = `
tenants:
  - slug: alpha
    installations:
      - name: alpha-bot
        forge: forgejo
        host: git.example
        account: alpha
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
  - slug: beta
    installations:
      - name: beta-bot
        forge: forgejo
        host: git.example
        account: beta
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
`

func testFile(t *testing.T) *configfile.File {
	t.Helper()
	t.Setenv("KRITIK_TEST_TOKEN", "tok")
	f, err := configfile.Parse([]byte(testConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func tenantIDOf(t *testing.T, f *configfile.File, slug string) string {
	t.Helper()
	tn, ok := f.Tenant(slug)
	if !ok {
		t.Fatalf("no tenant %s", slug)
	}
	return tn.ID()
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

func memberOf(t *testing.T, f *configfile.File, slug string, role auth.Role) *auth.Principal {
	t.Helper()
	return &auth.Principal{
		Account:     auth.Account{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com"},
		Memberships: map[string]auth.Role{tenantIDOf(t, f, slug): role},
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
	for _, path := range []string{"/api/v1/me", "/api/v1/tenants", "/api/v1/tenants/alpha/repos", "/api/events", "/api/nope"} {
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

func TestTenantScopeHidesUnreadableTenants(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	alphaMember := memberOf(t, ts.file, "alpha", auth.RoleMember)
	paths := []string{
		"/api/v1/tenants/%s", "/api/v1/tenants/%s/repos", "/api/v1/tenants/%s/repos/o/r", "/api/v1/tenants/%s/pulls",
		"/api/v1/tenants/%s/pulls/o/r/1", "/api/v1/tenants/%s/reviews/x", "/api/v1/tenants/%s/reviews/x/diff",
		"/api/v1/tenants/%s/reviews/x/transcript", "/api/v1/tenants/%s/reviews/x/raw", "/api/v1/tenants/%s/index-runs",
		"/api/v1/tenants/%s/followups", "/api/v1/tenants/%s/followups/1/transcript", "/api/v1/tenants/%s/usage",
		"/api/v1/tenants/%s/queue",
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

func TestResolveTenant(t *testing.T) {
	f := testFile(t)
	alpha := tenantIDOf(t, f, "alpha")
	tests := []struct {
		name     string
		p        *auth.Principal
		slug     string
		wantRole auth.Role
		wantErr  bool
	}{
		{"member reads own tenant", &auth.Principal{Memberships: map[string]auth.Role{alpha: auth.RoleMember}}, "alpha", auth.RoleMember, false},
		{"admin reads own tenant", &auth.Principal{Memberships: map[string]auth.Role{alpha: auth.RoleAdmin}}, "alpha", auth.RoleAdmin, false},
		{"member of another tenant", &auth.Principal{Memberships: map[string]auth.Role{alpha: auth.RoleMember}}, "beta", "", true},
		{"unknown tenant", &auth.Principal{Operator: true}, "gamma", "", true},
		{"operator reads any tenant as admin", &auth.Principal{Operator: true}, "beta", auth.RoleAdmin, false},
		{"no memberships", &auth.Principal{}, "alpha", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, err := resolveTenant(f, tt.p, tt.slug)
			if tt.wantErr {
				e, ok := err.(*apiError)
				if !ok || e.status != http.StatusNotFound {
					t.Fatalf("err = %v, want a 404", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTenant: %v", err)
			}
			if sc.tenant.Slug != tt.slug || sc.role() != tt.wantRole {
				t.Errorf("scope = %s as %q, want %s as %q", sc.tenant.Slug, sc.role(), tt.slug, tt.wantRole)
			}
		})
	}
}

func TestMe(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	tests := []struct {
		name     string
		p        *auth.Principal
		operator bool
		tenants  []TenantMembership
	}{
		{"member", memberOf(t, ts.file, "beta", auth.RoleMember), false,
			[]TenantMembership{{Slug: "beta", Role: auth.RoleMember, ManagedBy: configfile.OriginFile}}},
		{"operator sees every tenant as admin", &auth.Principal{Operator: true}, true, []TenantMembership{
			{Slug: "alpha", Role: auth.RoleAdmin, ManagedBy: configfile.OriginFile},
			{Slug: "beta", Role: auth.RoleAdmin, ManagedBy: configfile.OriginFile},
		}},
		{"no tenants", &auth.Principal{}, false, []TenantMembership{}},
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
			if me.Operator != tt.operator || len(me.Tenants) != len(tt.tenants) {
				t.Fatalf("me = %+v, want operator %v tenants %+v", me, tt.operator, tt.tenants)
			}
			for i := range tt.tenants {
				if me.Tenants[i] != tt.tenants[i] {
					t.Errorf("tenant %d = %+v, want %+v", i, me.Tenants[i], tt.tenants[i])
				}
			}
		})
	}
}

func TestListTenantsWithNoneReadable(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(&auth.Principal{}, httptest.NewRequest("GET", "/api/v1/tenants", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("got %d %q, want 200 []", w.Code, w.Body)
	}
}

func TestOperatorRouteHiddenFromNonOperators(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	w := ts.as(memberOf(t, ts.file, "alpha", auth.RoleAdmin), httptest.NewRequest("GET", "/api/v1/operator/tenants", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRequestValidation(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	p := memberOf(t, ts.file, "alpha", auth.RoleMember)
	tests := []struct {
		path string
		code ErrorCode
	}{
		{"/api/v1/tenants/alpha/repos?limit=0", CodeBadRequest},
		{"/api/v1/tenants/alpha/repos?cursor=@@", CodeInvalidCursor},
		{"/api/v1/tenants/alpha/pulls?state=merged", CodeBadRequest},
		{"/api/v1/tenants/alpha/pulls?outcome=great", CodeBadRequest},
		{"/api/v1/tenants/alpha/usage?group=week", CodeBadRequest},
		{"/api/v1/tenants/alpha/usage?from=yesterday", CodeBadRequest},
		{"/api/v1/tenants/alpha/usage?from=2026-02-01&to=2026-01-01", CodeBadRequest},
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
	p := &auth.Principal{Operator: true}
	for _, path := range []string{"/api/v1/nope", "/api/v2/tenants", "/auth/nope"} {
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
	w := ts.as(&auth.Principal{Operator: true}, httptest.NewRequest("POST", "/api/v1/me", nil))
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

func TestBasePath(t *testing.T) {
	ts := newTestServer(t, "https://example.com/kritik/")
	p := &auth.Principal{Operator: true}
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
