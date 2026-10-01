package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/home-operations/kritika/internal/configfile"
)

// githubAuth is an auth block with a GitHub sign-in whose client id is
// clientID.
func githubAuth(clientID string) string {
	return "auth:\n" + adminPassword + "  github:\n    clientId: " + clientID + "\n    clientSecret: { env: TEST_AUTH_SECRET }\n"
}

// oidcAuth is an auth block with an OIDC sign-in at issuer.
func oidcAuth(issuer string) string {
	return "auth:\n" + adminPassword + "  oidc:\n    issuer: " + issuer + "\n    clientId: c\n    clientSecret: { env: TEST_AUTH_SECRET }\n"
}

func TestForgeProviderURLs(t *testing.T) {
	s, _ := testFile(t, githubAuth("cid")).Auth.SignInByType(configfile.SignInGitHub)
	p, err := buildProvider(context.Background(), s, "https://kritika.example.com/auth/callback/github", http.DefaultClient, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	fp := p.(*forgeProvider)
	if fp.conf.Endpoint.TokenURL != "https://github.com/login/oauth/access_token" || fp.apiBase != "https://api.github.com" {
		t.Fatalf("token URL %q, API %q", fp.conf.Endpoint.TokenURL, fp.apiBase)
	}
	u, err := url.Parse(p.AuthCodeURL("st", "no", "verifier-verifier-verifier-verifier-verifier"))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://github.com/login/oauth/authorize" {
		t.Fatalf("authorize URL %q", got)
	}
	q := u.Query()
	want := map[string]string{
		"client_id": "cid", "state": "st", "response_type": "code", "scope": "read:user user:email read:org",
		"redirect_uri":          "https://kritika.example.com/auth/callback/github",
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("query %s = %q, want %q (url %s)", k, q.Get(k), v, u)
		}
	}
	if q.Get("code_challenge") == "" || q.Has("code_verifier") || q.Has("nonce") {
		t.Fatalf("PKCE parameters wrong in %s", u)
	}
}

func TestRedirectURL(t *testing.T) {
	tests := []struct{ web, want string }{
		{"https://kritika.example.com", "https://kritika.example.com/auth/callback/gh"},
		{"https://kritika.example.com/", "https://kritika.example.com/auth/callback/gh"},
		{"https://example.com/kritika/", "https://example.com/kritika/auth/callback/gh"},
	}
	for _, tt := range tests {
		t.Run(tt.web, func(t *testing.T) {
			u, _ := url.Parse(tt.web)
			if got := redirectURL(u, "gh"); got != tt.want {
				t.Fatalf("redirectURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProvidersCacheRebuildsOnChange(t *testing.T) {
	auth := testFile(t, githubAuth("one")).Auth
	u, _ := url.Parse("https://kritika.example.com")
	ps := newProviders(u, http.DefaultClient, nil)
	a, _, err := ps.get(context.Background(), auth, "github")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := ps.get(context.Background(), auth, "github")
	if a != b {
		t.Fatal("unchanged config rebuilt the provider")
	}
	auth = testFile(t, githubAuth("two")).Auth
	c, _, _ := ps.get(context.Background(), auth, "github")
	if c == a || !strings.Contains(c.AuthCodeURL("s", "n", "v"), "client_id=two") {
		t.Fatal("changed config did not rebuild the provider")
	}
	for _, name := range []string{"oidc", "local", "nope"} {
		if _, _, err := ps.get(context.Background(), auth, name); !errors.Is(err, ErrUnknownProvider) {
			t.Fatalf("get(%s) = %v, want ErrUnknownProvider", name, err)
		}
	}
}

func TestSignInOrigin(t *testing.T) {
	gh, _ := testFile(t, githubAuth("c")).Auth.SignInByType(configfile.SignInGitHub)
	oidc, _ := testFile(t, oidcAuth("https://id.example.com/realms/a")).Auth.SignInByType(configfile.SignInOIDC)
	if got := signInOrigin(gh); got != "github:https://github.com" {
		t.Fatalf("github origin = %q", got)
	}
	if got := signInOrigin(oidc); got != "oidc:https://id.example.com/realms/a" {
		t.Fatalf("oidc origin = %q", got)
	}
}

func TestProvidersRemembersFailedDiscovery(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	now := time.Now()
	u, _ := url.Parse("https://kritika.example.com")
	ps := newProviders(u, srv.Client(), func() time.Time { return now })
	auth := testFile(t, oidcAuth(srv.URL)).Auth
	for range 3 {
		if _, _, err := ps.get(context.Background(), auth, "oidc"); err == nil || errors.Is(err, ErrUnknownProvider) {
			t.Fatalf("get = %v, want a discovery error", err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("discovery requests = %d within the retry window, want 1", hits.Load())
	}
	now = now.Add(failedBuildTTL)
	_, _, _ = ps.get(context.Background(), auth, "oidc")
	if hits.Load() != 2 {
		t.Fatalf("discovery requests = %d after the retry window, want 2", hits.Load())
	}
}

func TestProvidersDoesNotRememberAnEndedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	u, _ := url.Parse("https://kritika.example.com")
	ps := newProviders(u, srv.Client(), nil)
	auth := testFile(t, oidcAuth(srv.URL)).Auth
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ps.get(canceled, auth, "oidc"); err == nil {
		t.Fatal("get with a canceled request succeeded")
	}
	if _, _, err := ps.get(context.Background(), auth, "oidc"); err == nil || hits.Load() != 1 {
		t.Fatalf("get after a canceled request = %v with %d discovery requests, want a fresh attempt", err, hits.Load())
	}
}

func TestProvidersRemembersASlowIssuer(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	client := srv.Client()
	client.Timeout = 50 * time.Millisecond
	u, _ := url.Parse("https://kritika.example.com")
	ps := newProviders(u, client, nil)
	auth := testFile(t, oidcAuth(srv.URL)).Auth
	if _, _, err := ps.get(context.Background(), auth, "oidc"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get = %v, want the client timeout", err)
	}
	start := time.Now()
	if _, _, err := ps.get(context.Background(), auth, "oidc"); err == nil {
		t.Fatal("second get succeeded")
	}
	if hits.Load() != 1 || time.Since(start) >= client.Timeout {
		t.Fatalf("second get hit the issuer (%d requests, %s): a client timeout was not remembered", hits.Load(), time.Since(start))
	}
}
