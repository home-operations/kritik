// Package auth signs humans in to the dashboard, as the local admin or
// through the file's OIDC and GitHub sign-ins, decides what each may do from
// the sign-in's role mapping and the forge, keeps their sessions, and guards
// the dashboard's routes (ADR-0009 §2.7, ADR-0014 §2.5).
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
// sign-in's type and Origin where it points (see signInOrigin); Subject is
// the provider's stable id for them, unique within that origin.
// EmailVerified is the provider's own word: the OIDC email_verified claim,
// or the forge's verified flag on the primary address.
type Identity struct {
	Provider, Origin, Subject, Login, Email string
	EmailVerified                           bool
	DisplayName, AvatarURL                  string
}

// Provider is one configured way to sign in.
type Provider interface {
	// AuthCodeURL is where to send the browser to sign in. nonce binds an
	// OIDC ID token to this request; forge OAuth ignores it.
	AuthCodeURL(state, nonce, pkceVerifier string) string
	// Exchange trades the callback's code for the human's identity and the
	// facts their grant is decided from.
	Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, Facts, error)
}

// Facts are what a provider learned about a person, bound to their token,
// for deciding what they may do.
type Facts struct {
	// MappingVars fills the sign-in's role mapping's variables; it may call
	// the provider, so it runs only when the sign-in has a mapping.
	MappingVars func(ctx context.Context) (map[string]any, error)
	// Membership asks a forge about one organization; nil for OIDC.
	Membership Membership
}

// ErrUnknownProvider is a sign-in the file does not configure.
var ErrUnknownProvider = errors.New("auth: unknown sign-in")

// failedBuildTTL is how long a failed build, an unreachable OIDC issuer's
// discovery, is remembered before a sign-in retries it, so a dead issuer
// does not cost a network round trip on every attempt.
const failedBuildTTL = 30 * time.Second

// providers builds each sign-in's Provider on first use and keeps it until
// the file's sign-ins change. A failed build is remembered for
// failedBuildTTL.
type providers struct {
	webURL *url.URL
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	key    [sha256.Size]byte
	built  map[string]Provider
	failed map[string]failedBuild
}

type failedBuild struct {
	at  time.Time
	err error
}

func newProviders(webURL *url.URL, client *http.Client, now func() time.Time) *providers {
	if now == nil {
		now = time.Now
	}
	return &providers{webURL: webURL, client: client, now: now, built: map[string]Provider{}, failed: map[string]failedBuild{}}
}

// get returns the provider for the sign-in of type name and its
// configuration.
func (ps *providers) get(ctx context.Context, auth configfile.Auth, name string) (Provider, *configfile.SignIn, error) {
	signIn, ok := auth.SignInByType(configfile.SignInType(name))
	if !ok {
		return nil, nil, ErrUnknownProvider
	}
	key := signInsKey(auth.SignIns())
	ps.mu.Lock()
	if key != ps.key {
		ps.key = key
		ps.built = map[string]Provider{}
		ps.failed = map[string]failedBuild{}
	}
	p, ok := ps.built[name]
	failed, hasFailed := ps.failed[name]
	ps.mu.Unlock()
	if ok {
		return p, signIn, nil
	}
	if hasFailed && ps.now().Sub(failed.at) < failedBuildTTL {
		return nil, nil, failed.err
	}
	// Built outside the lock: OIDC discovery is a network round trip, and two
	// concurrent first builds only cost a duplicate discovery.
	p, err := buildProvider(ctx, signIn, redirectURL(ps.webURL, name), ps.client, ps.now)
	ps.mu.Lock()
	if ps.key == key {
		switch {
		case err == nil:
			ps.built[name] = p
			delete(ps.failed, name)
		case !requestEnded(ctx):
			ps.failed[name] = failedBuild{at: ps.now(), err: err}
		}
	}
	ps.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	return p, signIn, nil
}

// requestEnded reports whether a failed build is the caller's request going
// away rather than the provider failing, which says nothing about the
// provider and must not hold back the next sign-in. Only ctx decides: an
// http.Client timeout also matches context.DeadlineExceeded, yet it is the
// provider being slow.
func requestEnded(ctx context.Context) bool {
	return ctx.Err() != nil
}

// signInsKey fingerprints everything a built provider depends on, the
// resolved client secret included, so rotating it rebuilds.
func signInsKey(signIns []*configfile.SignIn) [sha256.Size]byte {
	h := sha256.New()
	for _, s := range signIns {
		fields := []string{string(s.Type()), s.Issuer, s.ClientID, s.ClientSecretValue().Value(), strings.Join(s.Scopes, " ")}
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

func buildProvider(
	ctx context.Context, s *configfile.SignIn, redirect string, client *http.Client, now func() time.Time,
) (Provider, error) {
	switch s.Type() {
	case configfile.SignInOIDC:
		return newOIDCProvider(ctx, s, redirect, client, now)
	case configfile.SignInGitHub:
		return newGitHubProvider(s, redirect, client), nil
	default:
		return nil, fmt.Errorf("auth: unsupported sign-in type %q", s.Type())
	}
}

// signInOrigin is where a sign-in points: its type and base URL, or for OIDC
// its issuer exactly as configured, since the issuer is compared exactly
// against the ID token's. Identities and sessions are bound to it.
func signInOrigin(s *configfile.SignIn) string {
	if s.Type() == configfile.SignInOIDC {
		return string(s.Type()) + ":" + s.Issuer
	}
	return string(s.Type()) + ":https://" + configfile.GitHubHost
}

// localOrigin is the local admin's origin: it has no provider to point at.
const localOrigin = "local"

// redirectURL is the callback a provider returns the browser to.
func redirectURL(webURL *url.URL, name string) string {
	return strings.TrimSuffix(webURL.String(), "/") + "/auth/callback/" + url.PathEscape(name)
}
