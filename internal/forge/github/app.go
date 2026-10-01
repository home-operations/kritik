// Package github is kritika's forge: the App's credentials, a transport
// that signs requests as the App with a short-lived JWT and one that mints,
// caches and refreshes an installation token, the forge.Client over an
// installation, and the App manifest flow. The installation token
// authenticates API calls and git fetches alike, so an App-only
// installation needs no personal token.
package github

import (
	"context"
	"crypto/rsa"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	gh "github.com/google/go-github/v92/github"
)

// App is a GitHub App identity: the client id and private key of one App.
type App struct {
	clientID string
	apiBase  string // "" for api.github.com
	apps     *gh.Client

	// tokens holds one InstallationTokens per installation, so every
	// client and listing of an installation shares its token instead of
	// minting one each.
	mu     sync.Mutex
	tokens map[int64]*InstallationTokens
}

// NewApp parses the App's private key and builds the App-level client used
// for installation lookup and token minting. apiBase is empty for
// api.github.com; tests point it at a server of their own.
func NewApp(clientID, privateKeyPEM, apiBase string) (*App, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("github: parse App private key: %w", err)
	}
	a := &App{clientID: clientID, apiBase: apiBase}
	client, err := newClient(&appJWTTransport{base: http.DefaultTransport, clientID: clientID, key: key}, apiBase)
	if err != nil {
		return nil, err
	}
	a.apps = client
	return a, nil
}

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

// InstallationTokens returns the token source for one installation, as
// DiscoverInstallation finds it: the same one on every call, so the token
// is minted once an hour however often the installation is used.
func (a *App) InstallationTokens(installationID int64) *InstallationTokens {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.tokens[installationID]; ok {
		return t
	}
	if a.tokens == nil {
		a.tokens = map[int64]*InstallationTokens{}
	}
	t := &InstallationTokens{apps: a.apps, instID: installationID}
	a.tokens[installationID] = t
	return t
}

// DiscoverInstallation returns the id of the App's installation on the
// account that owns the repository.
func (a *App) DiscoverInstallation(ctx context.Context, owner, repo string) (int64, error) {
	inst, _, err := a.apps.Apps.GetRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("github: find App installation for %s/%s: %w", owner, repo, err)
	}
	return inst.GetID(), nil
}

// Installation is one account the App is installed on.
type Installation struct {
	ID int64
	// Account is the login of the account, AccountType "User" or
	// "Organization".
	Account, AccountType string
	// AllRepositories is whether it covers every repository of the account
	// rather than those selected.
	AllRepositories bool
	Suspended       bool
	// HTMLURL is the installation's settings page on GitHub.
	HTMLURL string
}

// Installations lists every account the App is installed on.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	var out []Installation
	for inst, err := range a.apps.Apps.ListInstallationsIter(ctx, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("github: list App installations: %w", err)
		}
		out = append(out, Installation{
			ID: inst.GetID(), Account: inst.GetAccount().GetLogin(), AccountType: inst.GetAccount().GetType(),
			AllRepositories: inst.GetRepositorySelection() == "all", Suspended: inst.SuspendedAt != nil, HTMLURL: inst.GetHTMLURL(),
		})
	}
	return out, nil
}

// Repository is one repository an installation reaches.
type Repository struct {
	// Name is the repository's name, FullName "owner/name".
	Name, FullName, DefaultBranch string
	Archived, Fork                bool
}

// Repositories lists every repository installation id reaches, with a
// token of that installation.
func (a *App) Repositories(ctx context.Context, id int64) ([]Repository, error) {
	client, err := a.Client(a.InstallationTokens(id))
	if err != nil {
		return nil, err
	}
	var out []Repository
	for r, err := range client.Apps.ListReposIter(ctx, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("github: list installation %d repositories: %w", id, err)
		}
		out = append(out, Repository{
			Name: r.GetName(), FullName: r.GetFullName(), DefaultBranch: r.GetDefaultBranch(),
			Archived: r.GetArchived(), Fork: r.GetFork(),
		})
	}
	return out, nil
}

// AccountRepositories is what an App reaches on one account: whether it
// is installed there, and the repositories its installation reaches.
type AccountRepositories struct {
	Account      string
	Installed    bool
	Repositories []Repository
}

// Reach lists, for each of accounts in order, whether the App is installed
// there and the repositories it reaches. A suspended installation reaches
// none: it can mint no token to list them with.
func (a *App) Reach(ctx context.Context, accounts []string) ([]AccountRepositories, error) {
	insts, err := a.Installations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AccountRepositories, 0, len(accounts))
	for _, account := range accounts {
		entry := AccountRepositories{Account: account, Repositories: []Repository{}}
		for _, inst := range insts {
			if inst.Suspended || !strings.EqualFold(inst.Account, account) {
				continue
			}
			repos, err := a.Repositories(ctx, inst.ID)
			if err != nil {
				return nil, err
			}
			entry.Installed = true
			entry.Repositories = append(entry.Repositories, repos...)
		}
		out = append(out, entry)
	}
	return out, nil
}

// Uninstall removes the App's installation id.
func (a *App) Uninstall(ctx context.Context, id int64) error {
	if _, err := a.apps.Apps.DeleteInstallation(ctx, id); err != nil {
		return fmt.Errorf("github: uninstall App installation %d: %w", id, err)
	}
	return nil
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

// Token returns a valid installation token, minting one if needed.
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

// ReadOnly mints a token that can only read repo's contents and metadata,
// uncached, for a runner: the commands an agent runs there can read it, so
// it must not carry the App's other permissions or reach its other
// repositories.
func (t *InstallationTokens) ReadOnly(ctx context.Context, repo string) (string, error) {
	it, _, err := t.apps.Apps.CreateInstallationToken(ctx, t.instID, &gh.InstallationTokenOptions{
		Repositories: []string{repo},
		Permissions:  &gh.InstallationPermissions{Contents: new("read"), Metadata: new("read")},
	})
	if err != nil {
		return "", fmt.Errorf("github: mint a read-only token for %s on installation %d: %w", repo, t.instID, err)
	}
	return it.GetToken(), nil
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
