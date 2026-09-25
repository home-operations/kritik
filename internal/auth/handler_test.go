package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestReturnTo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "#/"},
		{"#/", "#/"},
		{"#/tenants/alpha/reviews", "#/tenants/alpha/reviews"},
		{"#/a_b.c~d-e%20f", "#/a_b.c~d-e%20f"},
		{"https://evil.example/", "#/"},
		{"//evil.example", "#/"},
		{"/tenants", "#/"},
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
	tests := []struct {
		web, path, loginPath string
		secure               bool
	}{
		{"https://kritik.example.com", "/", "/auth/callback", true},
		{"https://kritik.example.com/", "/", "/auth/callback", true},
		{"https://example.com/kritik/", "/kritik", "/kritik/auth/callback", true},
		{"http://localhost:8080", "/", "/auth/callback", false},
		{"http://localhost:8080/dash", "/dash", "/dash/auth/callback", false},
	}
	for _, tt := range tests {
		t.Run(tt.web, func(t *testing.T) {
			u, _ := url.Parse(tt.web)
			c := sessionCookie(u, "tok", expires)
			if c.Name != CookieName || c.Value != "tok" || c.Path != tt.path || c.Secure != tt.secure ||
				!c.HttpOnly || c.SameSite != http.SameSiteLaxMode || !c.Expires.Equal(expires) {
				t.Fatalf("cookie = %+v", c)
			}
			gone := clearedCookie(u)
			if gone.Name != CookieName || gone.Value != "" || gone.MaxAge >= 0 || gone.Path != tt.path || gone.Secure != tt.secure || !gone.HttpOnly {
				t.Fatalf("cleared cookie = %+v", gone)
			}
			lc := loginCookie(u, "browser")
			if lc.Name != "kritik_login" || lc.Value != "browser" || lc.Path != tt.loginPath || lc.Secure != tt.secure ||
				!lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode || lc.MaxAge != 600 {
				t.Fatalf("login cookie = %+v", lc)
			}
			if lg := clearedLoginCookie(u); lg.Name != "kritik_login" || lg.MaxAge >= 0 || lg.Path != tt.loginPath {
				t.Fatalf("cleared login cookie = %+v", lg)
			}
		})
	}
}

func TestProvidersEndpoint(t *testing.T) {
	f := &configfile.File{Web: configfile.Web{SignIn: []configfile.SignIn{
		{Name: "gh", Type: configfile.SignInGitHub, Host: "github.com"},
		{Name: "corp", Type: configfile.SignInOIDC, Issuer: "https://id.example.com"},
		{Name: "fj", Type: configfile.SignInForgejo, Host: "code.example.org"},
	}}}
	h := testHandler(t, "https://kritik.example.com", f)
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
		{Name: "gh", Type: configfile.SignInGitHub, DisplayName: "GitHub"},
		{Name: "corp", Type: configfile.SignInOIDC, DisplayName: "corp"},
		{Name: "fj", Type: configfile.SignInForgejo, DisplayName: "Forgejo (code.example.org)"},
	}
	if len(got) != len(want) {
		t.Fatalf("providers = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("providers[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	f.Web.SignIn = nil
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("no sign-ins = %s, want []", w.Body.String())
	}
}

func TestLoginUnknownProvider(t *testing.T) {
	h := testHandler(t, "https://kritik.example.com", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/login/nope", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_sign_in") ||
		!strings.Contains(w.Body.String(), `href="https://kritik.example.com/"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestLogoutNeedsSameOrigin(t *testing.T) {
	h := testHandler(t, "https://kritik.example.com", nil)
	mux := http.NewServeMux()
	h.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
