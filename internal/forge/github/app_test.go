package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestAppJWTClaims(t *testing.T) {
	key, _ := testKeyPEM(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	tok, err := appJWT("Iv1.abc", key, now)
	if err != nil {
		t.Fatal(err)
	}
	var claims jwt.RegisteredClaims
	parsed, err := jwt.ParseWithClaims(tok, &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !parsed.Valid {
		t.Fatalf("parse: %v", err)
	}
	if claims.Issuer != "Iv1.abc" {
		t.Fatalf("issuer = %q", claims.Issuer)
	}
	if got := claims.IssuedAt.Time; !got.Equal(now.Add(-30 * time.Second)) {
		t.Fatalf("iat = %v, want 30s backdated", got)
	}
	if got := claims.ExpiresAt.Time; !got.Equal(now.Add(9 * time.Minute)) {
		t.Fatalf("exp = %v, want now+9m (under GitHub's 10m cap)", got)
	}
}

func TestNewAppRejectsBadKey(t *testing.T) {
	if _, err := NewApp("Iv1.abc", "not a key", ""); err == nil {
		t.Fatal("expected an error for an unparsable key")
	}
}

func TestInstallationTokensMintOnceAndRefresh(t *testing.T) {
	_, pemKey := testKeyPEM(t)
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("mint request without an App JWT bearer: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v3/app/installations/42/access_tokens" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		mints++
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		if mints == 1 {
			exp = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339) // expires soon: forces a refresh
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test` + string(rune('0'+mints)) + `","expires_at":"` + exp + `"}`))
	}))
	defer srv.Close()

	app, err := NewApp("Iv1.abc", pemKey, srv.URL+"/api/v3")
	if err != nil {
		t.Fatal(err)
	}
	tokens := app.InstallationTokens(42)
	ctx := t.Context()
	first, err := tokens.Token(ctx)
	if err != nil || first != "ghs_test1" {
		t.Fatalf("first token = %q, %v", first, err)
	}
	// The first token expires within the refresh margin, so the next call
	// mints again; the second token is good for an hour and is then cached.
	second, _ := tokens.Token(ctx)
	third, _ := tokens.Token(ctx)
	if second != "ghs_test2" || third != "ghs_test2" || mints != 2 {
		t.Fatalf("tokens = %q, %q with %d mints; want a refresh then a cache hit", second, third, mints)
	}
}
