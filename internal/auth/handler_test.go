package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritika/internal/configfile"
)

func TestReturnTo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "#/"},
		{"#/", "#/"},
		{"#/accounts/alpha/reviews", "#/accounts/alpha/reviews"},
		{"#/a_b.c~d-e%20f", "#/a_b.c~d-e%20f"},
		{"https://evil.example/", "#/"},
		{"//evil.example", "#/"},
		{"/accounts", "#/"},
		{"#/x?y=1", "#/"},
		{"#/x#y", "#/"},
		{"#/x\n", "#/"},
		{"#//evil.example", "#//evil.example"},
		{"javascript:alert(1)", "#/"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := returnTo(tt.in); got != tt.want {
				t.Fatalf("returnTo(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSessionCookie(t *testing.T) {
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	// __Host- (https at the root) binds a cookie to this host alone, with
	// Path=/, so a sibling subdomain can neither set nor shadow it; under a
	// path only __Secure- is possible.
	tests := []struct {
		web, path, loginPath, session, login string
		secure                               bool
	}{
		{"https://kritika.example.com", "/", "/", "__Host-kritika_session", "__Host-kritika_login", true},
		{"https://kritika.example.com/", "/", "/", "__Host-kritika_session", "__Host-kritika_login", true},
		{"https://example.com/kritika/", "/kritika", "/kritika/auth/callback", "__Secure-kritika_session", "__Secure-kritika_login", true},
		{"http://localhost:8080", "/", "/auth/callback", "kritika_session", "kritika_login", false},
		{"http://localhost:8080/dash", "/dash", "/dash/auth/callback", "kritika_session", "kritika_login", false},
	}
	for _, tt := range tests {
		t.Run(tt.web, func(t *testing.T) {
			u, _ := url.Parse(tt.web)
			if got := SessionCookieName(u); got != tt.session {
				t.Fatalf("SessionCookieName = %q, want %q", got, tt.session)
			}
			c := sessionCookie(u, "tok", expires)
			if c.Name != tt.session || c.Value != "tok" || c.Path != tt.path || c.Secure != tt.secure || c.Domain != "" ||
				!c.HttpOnly || c.SameSite != http.SameSiteLaxMode || !c.Expires.Equal(expires) {
				t.Fatalf("cookie = %+v", c)
			}
			gone := clearedCookie(u)
			if gone.Name != tt.session || gone.Value != "" || gone.MaxAge >= 0 || gone.Path != tt.path || gone.Secure != tt.secure || !gone.HttpOnly {
				t.Fatalf("cleared cookie = %+v", gone)
			}
			lc := loginCookie(u, "browser")
			if lc.Name != tt.login || lc.Value != "browser" || lc.Path != tt.loginPath || lc.Secure != tt.secure ||
				!lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode || lc.MaxAge != 600 {
				t.Fatalf("login cookie = %+v", lc)
			}
			if lg := clearedLoginCookie(u); lg.Name != tt.login || lg.MaxAge >= 0 || lg.Path != tt.loginPath {
				t.Fatalf("cleared login cookie = %+v", lg)
			}
		})
	}
}

func TestProvidersEndpoint(t *testing.T) {
	f := testFile(t, "auth:\n"+adminPassword+"  oidc:\n    name: Corp\n    issuer: https://id.example.com\n    clientId: c\n"+
		"    clientSecret: { env: TEST_AUTH_SECRET }\n  github:\n    clientId: c\n    clientSecret: { env: TEST_AUTH_SECRET }\n")
	h := testHandler(t, "https://kritika.example.com", f)
	mux := http.NewServeMux()
	h.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got []ProviderInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []ProviderInfo{
		{Name: "local", Type: configfile.SignInLocal, DisplayName: "Admin"},
		{Name: "oidc", Type: configfile.SignInOIDC, DisplayName: "Corp"},
		{Name: "github", Type: configfile.SignInGitHub, DisplayName: "GitHub"},
	}
	if len(got) != len(want) {
		t.Fatalf("providers = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("providers[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	h.current.Set(&configfile.File{})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("no sign-ins = %s, want []", w.Body.String())
	}
}

func TestLoginUnknownProvider(t *testing.T) {
	h := testHandler(t, "https://kritika.example.com", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/login/local", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_sign_in") ||
		!strings.Contains(w.Body.String(), `href="https://kritika.example.com/"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestLogoutNeedsSameOrigin(t *testing.T) {
	h := testHandler(t, "https://kritika.example.com", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
