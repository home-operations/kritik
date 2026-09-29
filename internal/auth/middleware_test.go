package auth

import (
	"encoding/json"
	"errors"
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
	h, err := New(Config{Current: configfile.NewCurrent(f), WebURL: u})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
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
		{"POST origin with the default port spelled out", http.MethodPost, "1", "https://KRITIK.example.com:443", "", http.StatusTeapot},
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
	r = r.WithContext(WithPrincipal(r.Context(), &Principal{}))
	h.RequirePrincipal(next).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

func TestPrincipalCanRead(t *testing.T) {
	member := &Principal{Accounts: map[string]bool{"t1": true}}
	tests := []struct {
		name string
		p    *Principal
		want bool
	}{
		{"a member of the account", member, true},
		{"a member of another", &Principal{Accounts: map[string]bool{"t2": true}}, false},
		{"a member of every account", &Principal{AllAccounts: true}, true},
		{"an admin", &Principal{Admin: true}, true},
		{"nil principal", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.CanRead("t1"); got != tt.want {
				t.Fatalf("CanRead = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPrincipalFor: a member reads the accounts the grant names, among
// those a connection still serves.
func TestPrincipalFor(t *testing.T) {
	file := testFile(t, "auth:\n"+adminPassword)
	tid := func(name string) string {
		a, ok := file.Account(configfile.ForgeGitHub, name)
		if !ok {
			t.Fatalf("no account %s", name)
		}
		return a.ID()
	}
	sess := store.Session{
		User:     store.User{ID: "acct"},
		Identity: store.SignInIdentity{Provider: "github", Subject: "1", Login: "Alice"},
		Grant:    store.SessionGrant{Role: RoleMember, Accounts: []string{"github/widgets", "github/initech", "github/gone"}},
	}
	p := principalFor(file, sess)
	if p.Admin || p.AllAccounts || p.User.ID != "acct" || p.Identity.Login != "Alice" {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Accounts) != 2 || !p.Accounts[tid("widgets")] || !p.Accounts[tid("Initech")] {
		t.Fatalf("accounts = %v, want widgets and initech", p.Accounts)
	}
	sess.Grant = store.SessionGrant{Role: RoleAdmin}
	if p := principalFor(file, sess); !p.Admin || !p.CanRead(tid("acme")) {
		t.Fatalf("admin principal = %+v", p)
	}
}

// TestHonoured: a session stands while its sign-in is configured, points
// where it did, and would decide grants as it did.
func TestHonoured(t *testing.T) {
	auth := "auth:\n  admin: { password: { env: TEST_AUTH_SECRET } }\n  oidc:\n    issuer: https://id.example.com\n    clientId: k\n" +
		"    clientSecret: { env: TEST_AUTH_SECRET }\n    roleMapping: '\"a\" in roles ? \"admin\" : \"\"'\n"
	file := testFile(t, auth)
	s, _ := file.Auth.SignInByType(configfile.SignInOIDC)
	oidcKey, _ := GrantKey(file.Auth, "oidc")
	localKey, _ := GrantKey(file.Auth, "local")
	sess := func(provider, origin, key string) store.Session {
		return store.Session{Identity: store.SignInIdentity{Provider: provider, Origin: origin}, Grant: store.SessionGrant{Key: key}}
	}
	for _, tt := range []struct {
		name string
		sess store.Session
		want bool
	}{
		{"an oidc session", sess("oidc", signInOrigin(s), oidcKey), true},
		{"the local admin", sess("local", localOrigin, localKey), true},
		{"an issuer since moved", sess("oidc", "oidc:https://other.example.com", oidcKey), false},
		{"a mapping since changed", sess("oidc", signInOrigin(s), "old"), false},
		{"a sign-in since removed", sess("github", "github:https://github.com", oidcKey), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := honoured(file.Auth, tt.sess); got != tt.want {
				t.Fatalf("honoured = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeOrigin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://Kritik.Example.com", "https://kritik.example.com"},
		{"https://kritik.example.com:443", "https://kritik.example.com"},
		{"http://kritik.example.com:80", "http://kritik.example.com"},
		{"http://kritik.example.com:443", "http://kritik.example.com:443"},
		{"https://kritik.example.com:8443", "https://kritik.example.com:8443"},
		{"https://[::1]:443", "https://[::1]"},
		{"https://[::1]:8443", "https://[::1]:8443"},
		{"null", ""},
		{"", ""},
		{"ftp://kritik.example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeOrigin(tt.in); got != tt.want {
				t.Fatalf("normalizeOrigin(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewRejectsBadWebURL(t *testing.T) {
	for _, raw := range []string{"", "kritik.example.com", "/dash/", "ftp://kritik.example.com", "https://", "https://:443/", "mailto:ops@example.com"} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(Config{Current: configfile.NewCurrent(&configfile.File{}), WebURL: u}); !errors.Is(err, ErrWebURL) {
				t.Fatalf("New(%q) = %v, want ErrWebURL", raw, err)
			}
		})
	}
	if _, err := New(Config{Current: configfile.NewCurrent(&configfile.File{})}); !errors.Is(err, ErrWebURL) {
		t.Fatalf("New without a WebURL = %v, want ErrWebURL", err)
	}
}

func TestSameOriginEmptyOriginNeverMatches(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodPost, "/api/x", nil)
	r.Header.Set("X-Kritik", "1")
	w := httptest.NewRecorder()
	h.SameOrigin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("reached next") })).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
