// Package auth signs humans in to the dashboard through the file's web
// sign-ins, keeps their sessions, derives their tenant memberships from the
// forge, and guards the dashboard's routes (ADR-0009 §§2.3, 2.4, 2.7).
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
)

// Identity is who a sign-in provider says a human is. Provider is the
// sign-in's configured name, Subject the provider's stable id for them.
type Identity struct {
	Provider, Subject, Login, Email string
	EmailVerified                   bool
	DisplayName, AvatarURL          string
}

// Provider is one configured way to sign in.
type Provider interface {
	Name() string
	Type() configfile.SignInType
	DisplayName() string
	// AuthCodeURL is where to send the browser to sign in. nonce binds an
	// OIDC ID token to this request; forge OAuth ignores it.
	AuthCodeURL(state, nonce, pkceVerifier string) string
	// Exchange trades the callback's code for the human's identity and, for
	// a forge, a Membership bound to their token; OIDC returns a nil one.
	Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, Membership, error)
}

// ErrUnknownProvider is a sign-in name the file does not declare.
var ErrUnknownProvider = errors.New("auth: unknown sign-in")

// providers builds each sign-in's Provider on first use and keeps it until
// the file's sign-ins change. A build that fails, an unreachable OIDC
// issuer's discovery, is not cached, so the next sign-in retries.
type providers struct {
	webURL *url.URL
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	key   [sha256.Size]byte
	built map[string]Provider
}

func newProviders(webURL *url.URL, client *http.Client, now func() time.Time) *providers {
	return &providers{webURL: webURL, client: client, now: now, built: map[string]Provider{}}
}

// get returns the provider for the named sign-in and its configuration.
func (ps *providers) get(ctx context.Context, web configfile.Web, name string) (Provider, configfile.SignIn, error) {
	signIn, ok := web.SignInByName(name)
	if !ok {
		return nil, configfile.SignIn{}, ErrUnknownProvider
	}
	key := signInsKey(web.SignIn)
	ps.mu.Lock()
	if key != ps.key {
		ps.key = key
		ps.built = map[string]Provider{}
	}
	p, ok := ps.built[name]
	ps.mu.Unlock()
	if ok {
		return p, signIn, nil
	}
	// Built outside the lock: OIDC discovery is a network round trip, and two
	// concurrent first builds only cost a duplicate discovery.
	p, err := buildProvider(ctx, signIn, redirectURL(ps.webURL, name), ps.client, ps.now)
	if err != nil {
		return nil, configfile.SignIn{}, err
	}
	ps.mu.Lock()
	if ps.key == key {
		ps.built[name] = p
	}
	ps.mu.Unlock()
	return p, signIn, nil
}

// signInsKey fingerprints everything a built provider depends on, the
// resolved client secret included, so rotating it rebuilds.
func signInsKey(signIns []configfile.SignIn) [sha256.Size]byte {
	h := sha256.New()
	for _, s := range signIns {
		fields := []string{s.Name, string(s.Type), s.Issuer, s.Host, s.ClientID, s.ClientSecretValue().Value(), strings.Join(s.Scopes, " ")}
		for _, f := range fields {
			h.Write([]byte(f))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	var key [sha256.Size]byte
	h.Sum(key[:0])
	return key
}

func buildProvider(ctx context.Context, s configfile.SignIn, redirect string, client *http.Client, now func() time.Time) (Provider, error) {
	switch s.Type {
	case configfile.SignInOIDC:
		return newOIDCProvider(ctx, s, redirect, client, now)
	case configfile.SignInGitHub:
		return newGitHubProvider(s, redirect, client), nil
	case configfile.SignInForgejo:
		return newForgejoProvider(s, redirect, client), nil
	default:
		return nil, fmt.Errorf("auth: sign-in %s: unsupported type %q", s.Name, s.Type)
	}
}

// redirectURL is the callback a provider returns the browser to.
func redirectURL(webURL *url.URL, name string) string {
	return strings.TrimSuffix(webURL.String(), "/") + "/auth/callback/" + url.PathEscape(name)
}

// displayName is how the sign-in page labels a sign-in.
func displayName(s configfile.SignIn) string {
	switch s.Type {
	case configfile.SignInGitHub:
		if h := forgeHost(string(s.Type), s.Host); h != githubHost {
			return "GitHub (" + h + ")"
		}
		return "GitHub"
	case configfile.SignInForgejo:
		return "Forgejo (" + forgeHost(string(s.Type), s.Host) + ")"
	default:
		return s.Name
	}
}

// webBase is the scheme and host a forge host names, https unless it carries
// its own scheme, without a trailing slash.
func webBase(host string) string {
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(strings.TrimRight(host, "/"))
	if err != nil {
		return strings.TrimRight(host, "/")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

// hostname is the hostname of a URL or bare host, without a port.
func hostname(s string) string {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
