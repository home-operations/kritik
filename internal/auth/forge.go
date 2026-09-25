package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/home-operations/kritik/internal/configfile"
)

// maxAPIBody bounds how much of a forge API response is read.
const maxAPIBody = 1 << 20

// ErrForgeAPI is a forge API call that failed or answered unexpectedly.
var ErrForgeAPI = errors.New("auth: forge API")

// forgeAPI is what differs between the forges kritik signs in through once
// the OAuth dance is done.
type forgeAPI interface {
	identity(ctx context.Context, c apiClient) (Identity, error)
	orgRole(ctx context.Context, c apiClient, login, org string) (Role, error)
}

// forgeProvider is an OAuth 2 authorization-code sign-in with PKCE against a
// forge, whose API then says who the user is and which organizations they
// belong to.
type forgeProvider struct {
	signIn  configfile.SignIn
	conf    *oauth2.Config
	client  *http.Client
	apiBase string
	headers map[string]string
	api     forgeAPI
}

func newForgeProvider(
	s configfile.SignIn, web, apiBase, redirect string, scopes []string, client *http.Client, api forgeAPI,
) *forgeProvider {
	if len(s.Scopes) > 0 {
		scopes = s.Scopes
	}
	return &forgeProvider{
		signIn: s,
		conf: &oauth2.Config{
			ClientID:     s.ClientID,
			ClientSecret: s.ClientSecretValue().Value(),
			Endpoint:     oauth2.Endpoint{AuthURL: web + "/login/oauth/authorize", TokenURL: web + "/login/oauth/access_token"},
			RedirectURL:  redirect,
			Scopes:       scopes,
		},
		client:  client,
		apiBase: apiBase,
		api:     api,
	}
}

func (p *forgeProvider) Name() string                { return p.signIn.Name }
func (p *forgeProvider) Type() configfile.SignInType { return p.signIn.Type }
func (p *forgeProvider) DisplayName() string         { return displayName(p.signIn) }

func (p *forgeProvider) AuthCodeURL(state, _, pkceVerifier string) string {
	return p.conf.AuthCodeURL(state, oauth2.S256ChallengeOption(pkceVerifier))
}

func (p *forgeProvider) Exchange(ctx context.Context, code, pkceVerifier, _ string) (Identity, Membership, error) {
	tok, err := p.conf.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.client), code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, nil, fmt.Errorf("auth: %s: exchange: %w", p.signIn.Name, err)
	}
	c := apiClient{base: p.apiBase, token: tok.AccessToken, client: p.client, headers: p.headers}
	id, err := p.api.identity(ctx, c)
	if err != nil {
		return Identity{}, nil, fmt.Errorf("auth: %s: %w", p.signIn.Name, err)
	}
	id.Provider = p.signIn.Name
	if id.DisplayName == "" {
		id.DisplayName = id.Login
	}
	login := id.Login
	m := func(ctx context.Context, org string) (Role, error) {
		r, err := p.api.orgRole(ctx, c, login, org)
		if err != nil {
			return "", fmt.Errorf("auth: %s: organization %s: %w", p.signIn.Name, org, err)
		}
		return r, nil
	}
	return id, m, nil
}

// apiClient calls a forge's REST API as the signed-in user.
type apiClient struct {
	base    string
	token   string
	client  *http.Client
	headers map[string]string
}

// get fetches path and, on a 2xx answer, decodes its JSON body into v when v
// is non-nil. It returns the status for the caller to classify; only a
// transport or decoding failure is an error.
func (c apiClient) get(ctx context.Context, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrForgeAPI, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	for k, val := range c.headers {
		req.Header.Set(k, val)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: GET %s: %w", ErrForgeAPI, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxAPIBody)
	if resp.StatusCode/100 != 2 || v == nil {
		_, _ = io.Copy(io.Discard, body) // drain for connection reuse
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: GET %s: decode: %w", ErrForgeAPI, path, err)
	}
	return resp.StatusCode, nil
}

// getOK is get for a call that must succeed.
func (c apiClient) getOK(ctx context.Context, path string, v any) error {
	status, err := c.get(ctx, path, v)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("%w: GET %s: status %d", ErrForgeAPI, path, status)
	}
	return nil
}

// forgeEmail is one address from a forge's /user/emails.
type forgeEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// verifiedEmail sets id's email to the primary address when the forge has
// verified it. Without the scope to list addresses, the profile's own email
// stands, unverified: an unverified address can neither accept an invite nor
// match an email operator.
func verifiedEmail(ctx context.Context, c apiClient, id *Identity) error {
	var emails []forgeEmail
	status, err := c.get(ctx, "/user/emails", &emails)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return nil
	}
	for _, e := range emails {
		if e.Primary && e.Verified && e.Email != "" {
			id.Email, id.EmailVerified = e.Email, true
			return nil
		}
	}
	return nil
}

// notMember reports whether a status means the organization says the user
// is not in it, or will not tell them.
func notMember(status int) bool {
	return status == http.StatusNotFound || status == http.StatusForbidden
}
