// Package webhook verifies and parses inbound forge webhook requests.
// Verification uses the standard library (crypto/hmac, and subtle for
// GitLab's secret token), not the forge SDKs: GitLab's schemes have no SDK
// helper, Forgejo exposes none either, and crypto/hmac is the canonical,
// auditable primitive.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
)

// Verification outcomes. Callers map any non-nil error to HTTP 401.
var (
	ErrNoSecret          = errors.New("webhook: no secret configured")
	ErrMissingSignature  = errors.New("webhook: missing signature header")
	ErrSignatureMismatch = errors.New("webhook: signature mismatch")
)

// Verify reports whether an inbound webhook request is authentic for forge,
// given the installation's secret, the request headers, and the raw body.
// It returns nil when authentic; otherwise one of the sentinel errors above.
func Verify(forge configfile.Forge, secret string, header http.Header, body []byte) error {
	if secret == "" {
		return ErrNoSecret
	}
	switch forge {
	case configfile.ForgeGitHub:
		// X-Hub-Signature-256: "sha256=" + hex(HMAC-SHA256(body, secret)).
		return verifyHMAC(header.Get("X-Hub-Signature-256"), "sha256=", secret, body)
	case configfile.ForgeForgejo, configfile.ForgeGitea:
		// X-Gitea-Signature: hex(HMAC-SHA256(body, secret)), no prefix. Gitea
		// uses the same header name as Forgejo.
		return verifyHMAC(header.Get("X-Gitea-Signature"), "", secret, body)
	case configfile.ForgeGitLab:
		// A signing token signs each delivery; any other secret is GitLab's
		// secret token, which X-Gitlab-Token carries verbatim.
		if key, ok := strings.CutPrefix(secret, configfile.GitLabSigningTokenPrefix); ok {
			return verifyGitLabSignature(header, key, body)
		}
		return verifyToken(header.Get("X-Gitlab-Token"), secret)
	default:
		return fmt.Errorf("webhook: unsupported forge %q", forge)
	}
}

// verifyHMAC checks an HMAC-SHA256 signature header. prefix is stripped first
// (e.g. "sha256="); an empty prefix means the header is the bare hex digest.
// The comparison is constant-time (hmac.Equal).
func verifyHMAC(provided, prefix, secret string, body []byte) error {
	if provided == "" {
		return ErrMissingSignature
	}
	if prefix != "" {
		rest, ok := strings.CutPrefix(provided, prefix)
		if !ok {
			return ErrSignatureMismatch
		}
		provided = rest
	}
	got, err := hex.DecodeString(provided)
	if err != nil {
		return ErrSignatureMismatch
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrSignatureMismatch
	}
	return nil
}

// verifyToken checks a shared-secret token header with a constant-time compare.
func verifyToken(provided, secret string) error {
	if provided == "" {
		return ErrMissingSignature
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
		return ErrSignatureMismatch
	}
	return nil
}

// verifyGitLabSignature checks a delivery signed with a GitLab signing
// token, whose key is base64 after the prefix: webhook-signature lists
// "v1," and the base64 HMAC-SHA256 of "<webhook-id>.<webhook-timestamp>.<body>",
// space-separated, and one must match.
func verifyGitLabSignature(header http.Header, key string, body []byte) error {
	signatures := header.Get("webhook-signature")
	if signatures == "" {
		return ErrMissingSignature
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return ErrSignatureMismatch
	}
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte(header.Get("webhook-id") + "." + header.Get("webhook-timestamp") + "."))
	mac.Write(body)
	want := []byte("v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	for sig := range strings.FieldsSeq(signatures) {
		if hmac.Equal([]byte(sig), want) {
			return nil
		}
	}
	return ErrSignatureMismatch
}
