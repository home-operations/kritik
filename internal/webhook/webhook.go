// Package webhook verifies and parses inbound forge webhook requests.
// Verification uses crypto/hmac rather than a forge SDK: it is the
// canonical, auditable primitive, and one forge-neutral place to add
// another forge's scheme.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/home-operations/kritika/internal/configfile"
)

// Verification outcomes. Callers map any non-nil error to HTTP 401.
var (
	ErrNoSecret          = errors.New("webhook: no secret configured")
	ErrMissingSignature  = errors.New("webhook: missing signature header")
	ErrSignatureMismatch = errors.New("webhook: signature mismatch")
)

// Verify reports whether an inbound webhook request is authentic for forge,
// given the connection's secret, the request headers, and the raw body.
// It returns nil when authentic; otherwise one of the sentinel errors above.
func Verify(forge configfile.Forge, secret string, header http.Header, body []byte) error {
	if secret == "" {
		return ErrNoSecret
	}
	switch forge {
	case configfile.ForgeGitHub:
		// X-Hub-Signature-256: "sha256=" + hex(HMAC-SHA256(body, secret)).
		return verifyHMAC(header.Get("X-Hub-Signature-256"), secret, body)
	default:
		return fmt.Errorf("webhook: unsupported forge %q", forge)
	}
}

// Delivered reports whether the request carries the headers forge puts on
// every delivery, signed or not, so a request from anyone else can be told
// from one the forge sent.
func Delivered(forge configfile.Forge, header http.Header) bool {
	switch forge {
	case configfile.ForgeGitHub:
		return header.Get("X-GitHub-Delivery") != "" && header.Get("X-GitHub-Event") != ""
	default:
		return false
	}
}

// verifyHMAC checks a "sha256=" + hex HMAC-SHA256 signature header. The
// comparison is constant-time (hmac.Equal).
func verifyHMAC(provided, secret string, body []byte) error {
	if provided == "" {
		return ErrMissingSignature
	}
	digest, ok := strings.CutPrefix(provided, "sha256=")
	if !ok {
		return ErrSignatureMismatch
	}
	got, err := hex.DecodeString(digest)
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
