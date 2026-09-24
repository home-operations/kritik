// Package github holds the GitHub App credential machinery copied from
// konflate: a transport that signs requests as the App with a short-lived
// JWT, and one that mints, caches and refreshes an installation token. The
// installation token authenticates API calls and git fetches alike, so an
// App-only installation needs no personal token.
package github

import (
	"context"
	"crypto/rsa"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	gh "github.com/google/go-github/v92/github"
)

// App is a GitHub App identity: the client id and private key of one App,
// against one GitHub host.
type App struct {
	clientID string
	key      *rsa.PrivateKey
	apiBase  string // "" for github.com
	apps     *gh.Client
}

// NewApp parses the App's private key and builds the App-level client used
// for installation lookup and token minting. apiBase is empty for
// github.com and "https://host/api/v3" for GitHub Enterprise Server.
func NewApp(clientID, privateKeyPEM, apiBase string) (*App, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("github: parse App private key: %w", err)
	}
	a := &App{clientID: clientID, key: key, apiBase: apiBase}
	client, err := newClient(&appJWTTransport{base: http.DefaultTransport, clientID: clientID, key: key}, apiBase)
	if err != nil {
		return nil, err
	}
	a.apps = client
	return a, nil
}

// ClientID returns the App's client id.
func (a *App) ClientID() string { return a.clientID }

// Slug returns the App's URL slug, which names the bot user its comments
// are posted as.
func (a *App) Slug(ctx context.Context) (string, error) {
	app, _, err := a.apps.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("github: read App: %w", err)
	}
	if app.GetSlug() == "" {
		return "", fmt.Errorf("github: App %s has no slug", a.clientID)
	}
	return app.GetSlug(), nil
}

// InstallationTokens returns a token source for one installation. The
// installation id is known from the installation webhook; when it is not
// yet known, DiscoverInstallation finds it from a repository the App can see.
func (a *App) InstallationTokens(installationID int64) *InstallationTokens {
	return &InstallationTokens{apps: a.apps, instID: installationID}
}

// DiscoverInstallation returns the id of the App's installation on the
// repository, for installations declared before their webhook arrived.
func (a *App) DiscoverInstallation(ctx context.Context, owner, repo string) (int64, error) {
	inst, _, err := a.apps.Apps.GetRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("github: find App installation for %s/%s: %w", owner, repo, err)
	}
	return inst.GetID(), nil
}

// Client returns a go-github client authenticated as the installation.
func (a *App) Client(tokens *InstallationTokens) (*gh.Client, error) {
	return newClient(&installTransport{base: http.DefaultTransport, tokens: tokens}, a.apiBase)
}

func newClient(rt http.RoundTripper, apiBase string) (*gh.Client, error) {
	opts := []gh.ClientOptionsFunc{gh.WithTransport(rt)}
	if apiBase != "" {
		opts = append(opts, gh.WithEnterpriseURLs(apiBase, apiBase))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github: client: %w", err)
	}
	return c, nil
}

// appJWT mints a short-lived App JWT with the client id as issuer. The
// 9-minute lifetime stays under GitHub's 10-minute cap; the backdated iat
// absorbs minor clock skew.
func appJWT(clientID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    clientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(key)
}

// appJWTTransport signs each request as the App itself, regenerating the
// JWT as it nears expiry.
type appJWTTransport struct {
	base     http.RoundTripper
	clientID string
	key      *rsa.PrivateKey

	mu  sync.Mutex
	tok string
	exp time.Time
}

func (t *appJWTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.jwt()
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r)
}

func (t *appJWTTransport) jwt() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Until(t.exp) > time.Minute {
		return t.tok, nil
	}
	now := time.Now()
	tok, err := appJWT(t.clientID, t.key, now)
	if err != nil {
		return "", fmt.Errorf("github: sign App JWT: %w", err)
	}
	t.tok, t.exp = tok, now.Add(9*time.Minute)
	return tok, nil
}

// InstallationTokens mints an installation token on first use, caches it,
// and refreshes it before expiry. One instance per installation.
type InstallationTokens struct {
	apps   *gh.Client
	instID int64

	mu  sync.Mutex
	tok string
	exp time.Time
}

// Token returns a valid installation token, minting one if needed. It is
// also what the runner receives as its git credential.
func (t *InstallationTokens) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Until(t.exp) > time.Minute {
		return t.tok, nil
	}
	it, _, err := t.apps.Apps.CreateInstallationToken(ctx, t.instID, nil)
	if err != nil {
		return "", fmt.Errorf("github: mint installation token for %d: %w", t.instID, err)
	}
	t.tok, t.exp = it.GetToken(), it.GetExpiresAt().Time
	return t.tok, nil
}

// installTransport injects the installation token as the bearer.
type installTransport struct {
	base   http.RoundTripper
	tokens *InstallationTokens
}

func (t *installTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.tokens.Token(req.Context())
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r)
}
