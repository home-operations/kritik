package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/home-operations/kritik/internal/configfile"
)

// oidcScopes ask for the claims Identity is built from.
var oidcScopes = []string{oidc.ScopeOpenID, "profile", "email"}

// ErrOIDC is an ID token that is missing, invalid or not for this request.
var ErrOIDC = errors.New("auth: oidc")

// oidcProvider signs in through an OpenID Connect issuer with the
// authorization-code flow, PKCE and a nonce.
type oidcProvider struct {
	signIn   configfile.SignIn
	conf     *oauth2.Config
	client   *http.Client
	verifier *oidc.IDTokenVerifier
}

// newOIDCProvider runs the issuer's discovery, so it needs the network.
func newOIDCProvider(
	ctx context.Context, s configfile.SignIn, redirect string, client *http.Client, now func() time.Time,
) (*oidcProvider, error) {
	discovered, err := oidc.NewProvider(oidc.ClientContext(ctx, client), s.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: sign-in %s: discovery: %w", s.Name, err)
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
		client:   client,
		verifier: discovered.Verifier(&oidc.Config{ClientID: s.ClientID, Now: now}),
	}, nil
}

func (p *oidcProvider) Name() string                { return p.signIn.Name }
func (p *oidcProvider) Type() configfile.SignInType { return p.signIn.Type }
func (p *oidcProvider) DisplayName() string         { return displayName(p.signIn) }

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

func (p *oidcProvider) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, Membership, error) {
	tok, err := p.conf.Exchange(oidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, nil, fmt.Errorf("auth: %s: exchange: %w", p.signIn.Name, err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, nil, fmt.Errorf("%w: %s: token response has no id_token", ErrOIDC, p.signIn.Name)
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, nil, fmt.Errorf("%w: %s: %w", ErrOIDC, p.signIn.Name, err)
	}
	// go-oidc leaves the nonce to the caller.
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return Identity{}, nil, fmt.Errorf("%w: %s: nonce mismatch", ErrOIDC, p.signIn.Name)
	}
	var c idClaims
	if err := idt.Claims(&c); err != nil {
		return Identity{}, nil, fmt.Errorf("%w: %s: claims: %w", ErrOIDC, p.signIn.Name, err)
	}
	id := Identity{
		Provider: p.signIn.Name, Origin: signInOrigin(p.signIn), Subject: idt.Subject, Login: c.PreferredUsername,
		Email: c.Email, EmailVerified: bool(c.EmailVerified) && c.Email != "",
		DisplayName: c.Name, AvatarURL: c.Picture,
	}
	if id.DisplayName == "" {
		id.DisplayName = id.Login
	}
	return id, nil, nil
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
