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

	"github.com/home-operations/kritik/internal/configfile"
)

func TestForgeProviderURLs(t *testing.T) {
	tests := []struct {
		name      string
		signIn    configfile.SignIn
		authorize string
		token     string
		api       string
		scope     string
		display   string
	}{
		{
			name:      "github.com",
			signIn:    configfile.SignIn{Name: "gh", Type: configfile.SignInGitHub, Host: "github.com", ClientID: "cid"},
			authorize: "https://github.com/login/oauth/authorize",
			token:     "https://github.com/login/oauth/access_token",
			api:       "https://api.github.com",
			scope:     "read:user user:email read:org",
			display:   "GitHub",
		},
		{
			name:      "github enterprise",
			signIn:    configfile.SignIn{Name: "ghe", Type: configfile.SignInGitHub, Host: "GHE.example.com", ClientID: "cid", Scopes: []string{"read:user"}},
			authorize: "https://ghe.example.com/login/oauth/authorize",
			token:     "https://ghe.example.com/login/oauth/access_token",
			api:       "https://ghe.example.com/api/v3",
			scope:     "read:user",
			display:   "GitHub (ghe.example.com)",
		},
		{
			name:      "forgejo",
			signIn:    configfile.SignIn{Name: "fj", Type: configfile.SignInForgejo, Host: "code.example.org", ClientID: "cid"},
			authorize: "https://code.example.org/login/oauth/authorize",
			token:     "https://code.example.org/login/oauth/access_token",
			api:       "https://code.example.org/api/v1",
			scope:     "read:user read:organization",
			display:   "Forgejo (code.example.org)",
		},
		{
			name:      "forgejo with an explicit scheme and port",
			signIn:    configfile.SignIn{Name: "fj", Type: configfile.SignInForgejo, Host: "http://127.0.0.1:3000/", ClientID: "cid"},
			authorize: "http://127.0.0.1:3000/login/oauth/authorize",
			token:     "http://127.0.0.1:3000/login/oauth/access_token",
			api:       "http://127.0.0.1:3000/api/v1",
			scope:     "read:user read:organization",
			display:   "Forgejo (127.0.0.1)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := buildProvider(context.Background(), tt.signIn, "https://kritik.example.com/auth/callback/"+tt.signIn.Name, http.DefaultClient, nil)
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			fp := p.(*forgeProvider)
			if fp.conf.Endpoint.TokenURL != tt.token || fp.apiBase != tt.api {
				t.Fatalf("token URL %q, API %q; want %q, %q", fp.conf.Endpoint.TokenURL, fp.apiBase, tt.token, tt.api)
			}
			if p.Name() != tt.signIn.Name || p.Type() != tt.signIn.Type || p.DisplayName() != tt.display {
				t.Fatalf("name %q type %q display %q", p.Name(), p.Type(), p.DisplayName())
			}
			u, err := url.Parse(p.AuthCodeURL("st", "no", "verifier-verifier-verifier-verifier-verifier"))
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Scheme + "://" + u.Host + u.Path; got != tt.authorize {
				t.Fatalf("authorize URL %q, want %q", got, tt.authorize)
			}
			q := u.Query()
			want := map[string]string{
				"client_id": "cid", "state": "st", "response_type": "code", "scope": tt.scope,
				"redirect_uri":          "https://kritik.example.com/auth/callback/" + tt.signIn.Name,
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
		})
	}
}

func TestRedirectURL(t *testing.T) {
	tests := []struct{ web, want string }{
		{"https://kritik.example.com", "https://kritik.example.com/auth/callback/gh"},
		{"https://kritik.example.com/", "https://kritik.example.com/auth/callback/gh"},
		{"https://example.com/kritik/", "https://example.com/kritik/auth/callback/gh"},
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
	web := configfile.Web{SignIn: []configfile.SignIn{{Name: "gh", Type: configfile.SignInGitHub, Host: "github.com", ClientID: "one"}}}
	u, _ := url.Parse("https://kritik.example.com")
	ps := newProviders(u, http.DefaultClient, nil)
	a, _, err := ps.get(context.Background(), web, "gh")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := ps.get(context.Background(), web, "gh")
	if a != b {
		t.Fatal("unchanged config rebuilt the provider")
	}
	web.SignIn = []configfile.SignIn{{Name: "gh", Type: configfile.SignInGitHub, Host: "github.com", ClientID: "two"}}
	c, _, _ := ps.get(context.Background(), web, "gh")
	if c == a || !strings.Contains(c.AuthCodeURL("s", "n", "v"), "client_id=two") {
		t.Fatal("changed config did not rebuild the provider")
	}
	if _, _, err := ps.get(context.Background(), web, "nope"); err == nil {
		t.Fatal("unknown sign-in resolved")
	}
}

func TestSignInOrigin(t *testing.T) {
	tests := []struct {
		signIn configfile.SignIn
		want   string
	}{
		{configfile.SignIn{Type: configfile.SignInGitHub, Host: "github.com"}, "github:https://github.com"},
		{configfile.SignIn{Type: configfile.SignInGitHub, Host: "https://GitHub.com/"}, "github:https://github.com"},
		{configfile.SignIn{Type: configfile.SignInGitHub, Host: "GHE.example.com"}, "github:https://ghe.example.com"},
		{configfile.SignIn{Type: configfile.SignInForgejo, Host: "code.example.org"}, "forgejo:https://code.example.org"},
		{configfile.SignIn{Type: configfile.SignInForgejo, Host: "http://127.0.0.1:3000/"}, "forgejo:http://127.0.0.1:3000"},
		{configfile.SignIn{Type: configfile.SignInOIDC, Issuer: "https://id.example.com/realms/a"}, "oidc:https://id.example.com/realms/a"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := signInOrigin(tt.signIn); got != tt.want {
				t.Fatalf("signInOrigin = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProvidersRemembersFailedDiscovery(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	now := time.Now()
	u, _ := url.Parse("https://kritik.example.com")
	ps := newProviders(u, srv.Client(), func() time.Time { return now })
	web := configfile.Web{SignIn: []configfile.SignIn{{Name: "corp", Type: configfile.SignInOIDC, Issuer: srv.URL, ClientID: "c"}}}
	for range 3 {
		if _, _, err := ps.get(context.Background(), web, "corp"); err == nil || errors.Is(err, ErrUnknownProvider) {
			t.Fatalf("get = %v, want a discovery error", err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("discovery requests = %d within the retry window, want 1", hits.Load())
	}
	now = now.Add(failedBuildTTL)
	_, _, _ = ps.get(context.Background(), web, "corp")
	if hits.Load() != 2 {
		t.Fatalf("discovery requests = %d after the retry window, want 2", hits.Load())
	}
}

func TestProvidersDoesNotRememberAnEndedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	u, _ := url.Parse("https://kritik.example.com")
	ps := newProviders(u, srv.Client(), nil)
	web := configfile.Web{SignIn: []configfile.SignIn{{Name: "corp", Type: configfile.SignInOIDC, Issuer: srv.URL, ClientID: "c"}}}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ps.get(canceled, web, "corp"); err == nil {
		t.Fatal("get with a canceled request succeeded")
	}
	if _, _, err := ps.get(context.Background(), web, "corp"); err == nil || hits.Load() != 1 {
		t.Fatalf("get after a canceled request = %v with %d discovery requests, want a fresh attempt", err, hits.Load())
	}
}

func TestProvidersRemembersASlowIssuer(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	u, _ := url.Parse("https://kritik.example.com")
	ps := newProviders(u, client, nil)
	web := configfile.Web{SignIn: []configfile.SignIn{{Name: "corp", Type: configfile.SignInOIDC, Issuer: srv.URL, ClientID: "c"}}}
	if _, _, err := ps.get(context.Background(), web, "corp"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get = %v, want the client timeout", err)
	}
	start := time.Now()
	if _, _, err := ps.get(context.Background(), web, "corp"); err == nil {
		t.Fatal("second get succeeded")
	}
	if hits.Load() != 1 || time.Since(start) >= client.Timeout {
		t.Fatalf("second get hit the issuer (%d requests, %s): a client timeout was not remembered", hits.Load(), time.Since(start))
	}
}
