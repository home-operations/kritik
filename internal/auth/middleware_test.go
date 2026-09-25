package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

func testHandler(t *testing.T, webURL string, f *configfile.File) *Handler {
	t.Helper()
	u, err := url.Parse(webURL)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		f = &configfile.File{}
	}
	return New(Config{Current: configfile.NewCurrent(f), WebURL: u})
}

func TestSameOrigin(t *testing.T) {
	h := testHandler(t, "https://Kritik.Example.com/dash/", nil)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	tests := []struct {
		name, method, xKritik, origin, fetchSite string
		want                                     int
	}{
		{"GET needs nothing", http.MethodGet, "", "", "", http.StatusTeapot},
		{"HEAD needs nothing", http.MethodHead, "", "https://evil.example", "cross-site", http.StatusTeapot},
		{"POST with header and matching origin", http.MethodPost, "1", "https://kritik.example.com", "", http.StatusTeapot},
		{"POST with header and same-origin fetch site", http.MethodPost, "1", "", "same-origin", http.StatusTeapot},
		{"DELETE with header and origin", http.MethodDelete, "1", "https://kritik.example.com", "same-origin", http.StatusTeapot},
		{"POST without header", http.MethodPost, "", "https://kritik.example.com", "same-origin", http.StatusForbidden},
		{"POST with wrong header value", http.MethodPost, "true", "https://kritik.example.com", "", http.StatusForbidden},
		{"POST with header but no origin evidence", http.MethodPost, "1", "", "", http.StatusForbidden},
		{"POST cross-site origin", http.MethodPost, "1", "https://evil.example", "", http.StatusForbidden},
		{"POST origin on another scheme", http.MethodPost, "1", "http://kritik.example.com", "", http.StatusForbidden},
		{"POST origin on another port", http.MethodPost, "1", "https://kritik.example.com:8443", "", http.StatusForbidden},
		{"PUT same-site is not same-origin", http.MethodPut, "1", "", "same-site", http.StatusForbidden},
		{"PATCH cross-site fetch with a forged-looking origin", http.MethodPatch, "1", "null", "cross-site", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "https://kritik.example.com/dash/api/x", nil)
			if tt.xKritik != "" {
				r.Header.Set("X-Kritik", tt.xKritik)
			}
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			w := httptest.NewRecorder()
			h.SameOrigin(ok).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			if tt.want == http.StatusForbidden {
				assertCode(t, w, "csrf")
			}
		})
	}
}

func assertCode(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != code {
		t.Fatalf("body = %s (%v), want code %q", w.Body.String(), err, code)
	}
}

func TestRequirePrincipal(t *testing.T) {
	h := testHandler(t, "https://kritik.example.com", nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if PrincipalFrom(r.Context()) == nil {
			t.Fatal("next reached without a principal")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	w := httptest.NewRecorder()
	h.RequirePrincipal(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	assertCode(t, w, "unauthenticated")

	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r = r.WithContext(withPrincipal(r.Context(), &Principal{}))
	h.RequirePrincipal(next).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

func TestPrincipalRoles(t *testing.T) {
	member := &Principal{Memberships: map[string]Role{"t1": RoleMember, "t2": RoleAdmin}}
	operator := &Principal{Operator: true}
	var none *Principal
	tests := []struct {
		name              string
		p                 *Principal
		tenant            string
		canRead, canAdmin bool
	}{
		{"member reads", member, "t1", true, false},
		{"admin administers", member, "t2", true, true},
		{"stranger", member, "t3", false, false},
		{"operator everywhere", operator, "t3", true, true},
		{"nil principal", none, "t1", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.CanRead(tt.tenant); got != tt.canRead {
				t.Fatalf("CanRead = %v, want %v", got, tt.canRead)
			}
			if got := tt.p.CanAdmin(tt.tenant); got != tt.canAdmin {
				t.Fatalf("CanAdmin = %v, want %v", got, tt.canAdmin)
			}
		})
	}
}

func TestPrincipalFor(t *testing.T) {
	file := &configfile.File{
		Web: configfile.Web{
			SignIn:    []configfile.SignIn{{Name: "gh", Type: configfile.SignInGitHub}},
			Operators: []string{"gh:alice"},
		},
		Tenants: []configfile.Tenant{{Slug: "kept"}},
	}
	kept := file.Tenants[0].ID()
	sess := store.Session{
		Account:  store.Account{ID: "acct"},
		Identity: store.SignInIdentity{Provider: "gh", Subject: "1", Login: "Alice"},
	}
	p := principalFor(file, sess, map[string]Role{kept: RoleAdmin, "gone-tenant": RoleAdmin})
	if !p.Operator || p.Account.ID != "acct" || p.Identity.Login != "Alice" {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Memberships) != 1 || p.Memberships[kept] != RoleAdmin {
		t.Fatalf("memberships = %v, want only the tenant still in the file", p.Memberships)
	}
	file.Web.Operators = nil
	if principalFor(file, sess, nil).Operator {
		t.Fatal("operator removed from the file is still an operator")
	}
}
