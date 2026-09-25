//go:build integration

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	fakeClientID     = "kritik-client"
	fakeClientSecret = "tok" // KRITIK_TEST_TOKEN, which every clientSecret reads
)

// fakeUser is one account on a fake provider.
type fakeUser struct {
	ID            int64
	Login, Email  string
	EmailVerified bool
	// Orgs maps a lowercase organization to "admin", "member", or for
	// GitHub "pending"; absent means not a member.
	Orgs map[string]string
}

// pendingAuth is what the fake's authorize step would have recorded for a
// code: the PKCE challenge and nonce the browser was sent with.
type pendingAuth struct {
	challenge, nonce string
	user             *fakeUser
	// nonceOverride, when set, is put in the ID token instead of nonce.
	nonceOverride string
}

// fakeOAuth is the authorization-code half every fake provider shares.
type fakeOAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	codes  map[string]pendingAuth
	tokens map[string]*fakeUser
	idTok  func(p pendingAuth) string
}

// authorize records that user approved a sign-in whose authorize URL was
// location, and returns the code the provider would redirect back with.
func (f *fakeOAuth) authorize(location string, user *fakeUser, nonceOverride string) string {
	f.t.Helper()
	u := mustParseURL(f.t, location)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != fakeClientID {
		f.t.Fatalf("authorize URL %s lacks S256 PKCE or the client id", location)
	}
	code := "code-" + randomHex(f.t)
	f.mu.Lock()
	f.codes[code] = pendingAuth{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), user: user, nonceOverride: nonceOverride}
	f.mu.Unlock()
	return code
}

func (f *fakeOAuth) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	f.mu.Lock()
	p, found := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case id != fakeClientID || secret != fakeClientSecret:
		writeTokenError(w, "invalid_client")
		return
	case !found:
		writeTokenError(w, "invalid_grant")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge:
		writeTokenError(w, "invalid_grant")
		return
	}
	access := "at-" + randomHex(f.t)
	f.mu.Lock()
	f.tokens[access] = p.user
	f.mu.Unlock()
	resp := map[string]any{"access_token": access, "token_type": "bearer", "expires_in": 3600}
	if f.idTok != nil {
		resp["id_token"] = f.idTok(p)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeTokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// user returns the bearer token's user, or writes 401.
func (f *fakeOAuth) user(w http.ResponseWriter, r *http.Request) *fakeUser {
	f.mu.Lock()
	u := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return u
}

func newFakeOAuth(t *testing.T, mux *http.ServeMux) *fakeOAuth {
	f := &fakeOAuth{t: t, codes: map[string]pendingAuth{}, tokens: map[string]*fakeUser{}}
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newFakeOIDC is an OIDC issuer: discovery, JWKS, and a token endpoint
// that issues RS256 ID tokens carrying the authorize step's nonce.
func newFakeOIDC(t *testing.T) *fakeOAuth {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f := newFakeOAuth(t, mux)
	iss := f.srv.URL
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]any{
			"issuer": iss, "authorization_endpoint": iss + "/authorize", "token_endpoint": iss + "/token",
			"jwks_uri": iss + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", f.token)
	f.idTok = func(p pendingAuth) string {
		nonce := p.nonce
		if p.nonceOverride != "" {
			nonce = p.nonceOverride
		}
		now := time.Now()
		claims, _ := json.Marshal(map[string]any{
			"iss": iss, "sub": p.user.Login, "aud": fakeClientID, "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"nonce": nonce, "preferred_username": p.user.Login, "email": p.user.Email,
			// Some issuers send email_verified as a string.
			"email_verified": map[bool]string{true: "true", false: "false"}[p.user.EmailVerified], "name": "Name " + p.user.Login,
		})
		sig, err := signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := sig.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return f
}

// newFakeGitHub is a GitHub Enterprise Server: OAuth under /login/oauth, the
// REST API under /api/v3.
func newFakeGitHub(t *testing.T) *fakeOAuth {
	mux := http.NewServeMux()
	f := newFakeOAuth(t, mux)
	mux.HandleFunc("POST /login/oauth/access_token", f.token)
	mux.HandleFunc("GET /api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		if u := f.user(w, r); u != nil {
			writeFakeJSON(w, map[string]any{"id": u.ID, "login": u.Login, "name": "Name " + u.Login, "email": nil, "avatar_url": "https://a/" + u.Login})
		}
	})
	mux.HandleFunc("GET /api/v3/user/emails", func(w http.ResponseWriter, r *http.Request) {
		if u := f.user(w, r); u != nil {
			writeFakeJSON(w, []map[string]any{
				{"email": "other@" + u.Login + ".example", "primary": false, "verified": true},
				{"email": u.Email, "primary": true, "verified": u.EmailVerified},
			})
		}
	})
	mux.HandleFunc("GET /api/v3/user/memberships/orgs/{org}", func(w http.ResponseWriter, r *http.Request) {
		u := f.user(w, r)
		if u == nil {
			return
		}
		switch role := u.Orgs[strings.ToLower(r.PathValue("org"))]; role {
		case "":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case "pending":
			writeFakeJSON(w, map[string]string{"state": "pending", "role": "member"})
		default:
			writeFakeJSON(w, map[string]string{"state": "active", "role": role})
		}
	})
	return f
}

// newFakeForgejo is a Forgejo instance: OAuth2 under /login/oauth, the API
// under /api/v1.
func newFakeForgejo(t *testing.T) *fakeOAuth {
	mux := http.NewServeMux()
	f := newFakeOAuth(t, mux)
	mux.HandleFunc("POST /login/oauth/access_token", f.token)
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		if u := f.user(w, r); u != nil {
			writeFakeJSON(w, map[string]any{"id": u.ID, "login": u.Login, "full_name": "Name " + u.Login, "email": u.Email, "avatar_url": ""})
		}
	})
	mux.HandleFunc("GET /api/v1/user/emails", func(w http.ResponseWriter, r *http.Request) {
		if u := f.user(w, r); u != nil {
			writeFakeJSON(w, []map[string]any{{"email": u.Email, "primary": true, "verified": u.EmailVerified}})
		}
	})
	mux.HandleFunc("GET /api/v1/orgs/{org}/members/{username}", func(w http.ResponseWriter, r *http.Request) {
		u := f.user(w, r)
		if u == nil {
			return
		}
		if r.PathValue("username") != u.Login || u.Orgs[strings.ToLower(r.PathValue("org"))] == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/users/{username}/orgs/{org}/permissions", func(w http.ResponseWriter, r *http.Request) {
		u := f.user(w, r)
		if u == nil {
			return
		}
		role := u.Orgs[strings.ToLower(r.PathValue("org"))]
		writeFakeJSON(w, map[string]bool{"is_owner": role == "admin", "is_admin": false, "can_read": role != ""})
	})
	return f
}

// trustingClient trusts every fake's self-signed certificate.
func trustingClient(fakes ...*fakeOAuth) *http.Client {
	pool := x509.NewCertPool()
	for _, f := range fakes {
		pool.AddCert(f.srv.Certificate())
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}
