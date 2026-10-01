package auth

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/home-operations/kritika/internal/configfile"
)

// oidcScopes ask for the claims Identity is built from.
var oidcScopes = []string{oidc.ScopeOpenID, "profile", "email"}

// ErrOIDC is an ID token that is missing, invalid or not for this request.
var ErrOIDC = errors.New("auth: oidc")

// oidcProvider signs in through an OpenID Connect issuer with the
// authorization-code flow, PKCE and a nonce.
type oidcProvider struct {
	signIn     *configfile.SignIn
	conf       *oauth2.Config
	client     *http.Client
	discovered *oidc.Provider
	verifier   *oidc.IDTokenVerifier
}

// newOIDCProvider runs the issuer's discovery, so it needs the network.
func newOIDCProvider(
	ctx context.Context, s *configfile.SignIn, redirect string, client *http.Client, now func() time.Time,
) (*oidcProvider, error) {
	discovered, err := oidc.NewProvider(oidc.ClientContext(ctx, client), s.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc: discovery: %w", err)
	}
	scopes := oidcScopes
	if len(s.Scopes) > 0 {
		scopes = s.Scopes
		if !slices.Contains(scopes, oidc.ScopeOpenID) {
			scopes = append([]string{oidc.ScopeOpenID}, scopes...)
		}
	}
	return &oidcProvider{
		signIn: s,
		conf: &oauth2.Config{
			ClientID:     s.ClientID,
			ClientSecret: s.ClientSecretValue().Value(),
			Endpoint:     discovered.Endpoint(),
			RedirectURL:  redirect,
			Scopes:       scopes,
		},
		client:     client,
		discovered: discovered,
		verifier:   discovered.Verifier(&oidc.Config{ClientID: s.ClientID, Now: now}),
	}, nil
}

func (p *oidcProvider) AuthCodeURL(state, nonce, pkceVerifier string) string {
	return p.conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier))
}

// idClaims are the ID token claims Identity is built from. email_verified is
// a JSON bool, or a "true"/"false" string from some issuers.
type idClaims struct {
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	EmailVerified     flexBool `json:"email_verified"`
	Name              string   `json:"name"`
	Picture           string   `json:"picture"`
}

func (p *oidcProvider) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, Facts, error) {
	tok, err := p.conf.Exchange(oidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, Facts{}, fmt.Errorf("auth: oidc: exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, Facts{}, fmt.Errorf("%w: token response has no id_token", ErrOIDC)
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, Facts{}, fmt.Errorf("%w: %w", ErrOIDC, err)
	}
	// go-oidc leaves the nonce to the caller.
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return Identity{}, Facts{}, fmt.Errorf("%w: nonce mismatch", ErrOIDC)
	}
	var c idClaims
	if err := idt.Claims(&c); err != nil {
		return Identity{}, Facts{}, fmt.Errorf("%w: claims: %w", ErrOIDC, err)
	}
	id := Identity{
		Provider: string(p.signIn.Type()), Origin: signInOrigin(p.signIn), Subject: idt.Subject, Login: c.PreferredUsername,
		Email: c.Email, EmailVerified: bool(c.EmailVerified) && c.Email != "",
		DisplayName: c.Name, AvatarURL: c.Picture,
	}
	id.DisplayName = cmp.Or(id.DisplayName, id.Login)
	facts := Facts{MappingVars: func(ctx context.Context) (map[string]any, error) {
		claims := map[string]any{}
		if err := idt.Claims(&claims); err != nil {
			return nil, fmt.Errorf("%w: claims: %w", ErrOIDC, err)
		}
		if p.discovered.UserInfoEndpoint() != "" {
			info, err := p.discovered.UserInfo(oidc.ClientContext(ctx, p.client), oauth2.StaticTokenSource(tok))
			if err != nil {
				return nil, fmt.Errorf("%w: userinfo: %w", ErrOIDC, err)
			}
			more := map[string]any{}
			if err := info.Claims(&more); err != nil {
				return nil, fmt.Errorf("%w: userinfo: %w", ErrOIDC, err)
			}
			maps.Copy(claims, more)
		}
		return map[string]any{"claims": claims, "roles": rolesOf(claims[p.signIn.RolesClaim])}, nil
	}}
	return id, facts, nil
}

// rolesOf reads a roles claim: a list of strings, a map whose keys are the
// roles (as Zitadel sends them), or a single string. Anything else, or no
// claim, is no roles.
func rolesOf(v any) []string {
	roles := []string{}
	switch r := v.(type) {
	case []any:
		for _, x := range r {
			if s, ok := x.(string); ok {
				roles = append(roles, s)
			}
		}
	case map[string]any:
		roles = slices.Sorted(maps.Keys(r))
	case string:
		roles = append(roles, r)
	}
	return roles
}

type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v bool
	if err := json.Unmarshal(data, &v); err == nil {
		*b = flexBool(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("email_verified: %w", err)
	}
	*b = s == "true"
	return nil
}
