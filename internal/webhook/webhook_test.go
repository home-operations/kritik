package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

const canonicalBody = "Hello, World!"

func hmacSHA256Hex(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonicalBody))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestKnownVector pins the crypto against GitHub's published example from
// "Validating webhook deliveries".
func TestKnownVector(t *testing.T) {
	const (
		secret = "It's a Secret to Everybody"
		want   = "757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	)
	if got := hmacSHA256Hex(secret); got != want {
		t.Fatalf("HMAC-SHA256 hex = %s, want %s", got, want)
	}
}

// TestVerifyGitLabSignature checks GitLab's signing-token scheme against
// the Standard Webhooks reference vector, which GitLab's documented
// algorithm reproduces.
func TestVerifyGitLabSignature(t *testing.T) {
	const (
		secret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
		sig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	)
	body := []byte(`{"test": 2432232314}`)
	signed := func(signature string) http.Header {
		return http.Header{"Webhook-Id": {"msg_p5jXN8AQM9LWM0D4loKWxJek"}, "Webhook-Timestamp": {"1614265330"}, "Webhook-Signature": {signature}}
	}
	for _, tt := range []struct {
		name    string
		secret  string
		header  http.Header
		body    []byte
		wantErr error
	}{
		{"valid", secret, signed(sig), body, nil},
		{"one of several", secret, signed("v1,bm9wZQ== " + sig), body, nil},
		{"tampered body", secret, signed(sig), []byte(`{"test": 1}`), ErrSignatureMismatch},
		{"another delivery's id", secret, func() http.Header { h := signed(sig); h.Set("Webhook-Id", "msg_other"); return h }(), body, ErrSignatureMismatch},
		{"another key", "whsec_" + "c2VjcmV0", signed(sig), body, ErrSignatureMismatch},
		{"only the secret token", secret, http.Header{"X-Gitlab-Token": {secret}}, body, ErrMissingSignature},
		{"a key that is not base64", "whsec_!!", signed(sig), body, ErrSignatureMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := Verify(configfile.ForgeGitLab, tt.secret, tt.header, tt.body); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	const secret = "It's a Secret to Everybody"
	sig := hmacSHA256Hex(secret)
	body := []byte(canonicalBody)
	tests := []struct {
		name    string
		forge   configfile.Forge
		secret  string
		header  http.Header
		body    []byte
		wantErr error
	}{
		{"github valid", configfile.ForgeGitHub, secret, http.Header{"X-Hub-Signature-256": {"sha256=" + sig}}, body, nil},
		{"github wrong secret", configfile.ForgeGitHub, secret, http.Header{"X-Hub-Signature-256": {"sha256=" + hmacSHA256Hex("x")}}, body, ErrSignatureMismatch},
		{"github no prefix", configfile.ForgeGitHub, secret, http.Header{"X-Hub-Signature-256": {sig}}, body, ErrSignatureMismatch},
		{"github missing header", configfile.ForgeGitHub, secret, http.Header{}, body, ErrMissingSignature},
		{"github tampered body", configfile.ForgeGitHub, secret, http.Header{"X-Hub-Signature-256": {"sha256=" + sig}}, []byte("Goodbye"), ErrSignatureMismatch},
		{"github bad hex", configfile.ForgeGitHub, secret, http.Header{"X-Hub-Signature-256": {"sha256=zz"}}, body, ErrSignatureMismatch},
		{"forgejo valid", configfile.ForgeForgejo, secret, http.Header{"X-Gitea-Signature": {sig}}, body, nil},
		{"forgejo with prefix rejected", configfile.ForgeForgejo, secret, http.Header{"X-Gitea-Signature": {"sha256=" + sig}}, body, ErrSignatureMismatch},
		{"gitea valid", configfile.ForgeGitea, secret, http.Header{"X-Gitea-Signature": {sig}}, body, nil},
		{"gitea with prefix rejected", configfile.ForgeGitea, secret, http.Header{"X-Gitea-Signature": {"sha256=" + sig}}, body, ErrSignatureMismatch},
		{"gitlab valid", configfile.ForgeGitLab, secret, http.Header{"X-Gitlab-Token": {secret}}, body, nil},
		{"gitlab wrong token", configfile.ForgeGitLab, secret, http.Header{"X-Gitlab-Token": {"nope"}}, body, ErrSignatureMismatch},
		{"gitlab missing", configfile.ForgeGitLab, secret, http.Header{}, body, ErrMissingSignature},
		{"no secret configured", configfile.ForgeGitHub, "", http.Header{"X-Hub-Signature-256": {"sha256=" + sig}}, body, ErrNoSecret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.forge, tt.secret, tt.header, tt.body)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if err := Verify("bitbucket", secret, http.Header{}, body); err == nil {
		t.Fatal("unsupported forge must error")
	}
}
