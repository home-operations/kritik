package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/home-operations/kritika/internal/configfile"
)

// maxAPIBody bounds how much of a forge API response is read.
const maxAPIBody = 1 << 20

// ErrForgeAPI is a forge API call that failed or answered unexpectedly.
var ErrForgeAPI = errors.New("auth: forge API")

// forgeAPI is what differs between the forges kritika signs in through once
// the OAuth dance is done.
type forgeAPI interface {
	identity(ctx context.Context, c apiClient) (Identity, error)
	// member reports whether the user is an active member of org.
	member(ctx context.Context, c apiClient, org string) (bool, error)
	// mappingVars fills the role mapping's variables for the user.
	mappingVars(ctx context.Context, c apiClient, id Identity) (map[string]any, error)
}

// forgeProvider is an OAuth 2 authorization-code sign-in with PKCE against a
// forge, whose API then says who the user is and which organizations they
// belong to.
type forgeProvider struct {
	signIn  *configfile.SignIn
	conf    *oauth2.Config
	client  *http.Client
	apiBase string
	headers map[string]string
	api     forgeAPI
}

func newForgeProvider(
	s *configfile.SignIn, web, apiBase, redirect string, scopes []string, client *http.Client, api forgeAPI,
) *forgeProvider {
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

func (p *forgeProvider) AuthCodeURL(state, _, pkceVerifier string) string {
	return p.conf.AuthCodeURL(state, oauth2.S256ChallengeOption(pkceVerifier))
}

func (p *forgeProvider) Exchange(ctx context.Context, code, pkceVerifier, _ string) (Identity, Facts, error) {
	name := string(p.signIn.Type())
	tok, err := p.conf.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.client), code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, Facts{}, fmt.Errorf("auth: %s: exchange: %w", name, err)
	}
	c := apiClient{base: p.apiBase, token: tok.AccessToken, client: p.client, headers: p.headers}
	id, err := p.api.identity(ctx, c)
	if err != nil {
		return Identity{}, Facts{}, fmt.Errorf("auth: %s: %w", name, err)
	}
	id.Provider, id.Origin = name, signInOrigin(p.signIn)
	id.DisplayName = cmp.Or(id.DisplayName, id.Login)
	facts := Facts{
		MappingVars: func(ctx context.Context) (map[string]any, error) {
			vars, err := p.api.mappingVars(ctx, c, id)
			if err != nil {
				return nil, fmt.Errorf("auth: %s: %w", name, err)
			}
			return vars, nil
		},
		Membership: func(ctx context.Context, org string) (bool, error) {
			ok, err := p.api.member(ctx, c, org)
			if err != nil {
				return false, fmt.Errorf("auth: %s: organization %s: %w", name, org, err)
			}
			return ok, nil
		},
	}
	return id, facts, nil
}

// apiClient calls a forge's REST API as the signed-in user.
type apiClient struct {
	base    string
	token   string
	client  *http.Client
	headers map[string]string
}

// get fetches path and, on a 2xx answer, decodes its JSON body into v when v
// is non-nil. It returns the status and headers for the caller to classify;
// only a transport or decoding failure is an error.
func (c apiClient) get(ctx context.Context, path string, v any) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrForgeAPI, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	for k, val := range c.headers {
		req.Header.Set(k, val)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: GET %s: %w", ErrForgeAPI, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxAPIBody)
	if resp.StatusCode/100 != 2 || v == nil {
		_, _ = io.Copy(io.Discard, body) // drain for connection reuse
		return resp.StatusCode, resp.Header, nil
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return resp.StatusCode, resp.Header, fmt.Errorf("%w: GET %s: decode: %w", ErrForgeAPI, path, err)
	}
	return resp.StatusCode, resp.Header, nil
}

// getOK is get for a call that must succeed.
func (c apiClient) getOK(ctx context.Context, path string, v any) error {
	status, _, err := c.get(ctx, path, v)
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
// stands, unverified.
func verifiedEmail(ctx context.Context, c apiClient, id *Identity) error {
	var emails []forgeEmail
	status, _, err := c.get(ctx, "/user/emails", &emails)
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
