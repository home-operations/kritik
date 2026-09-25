package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/forge"
)

// newTestClient builds a Client whose underlying go-github API calls, and
// whose installation-token minting, both hit srv. It mirrors
// TestInstallationTokensMintOnceAndRefresh's server setup but leaves the
// caller free to install its own handler for the actual API request under
// test; the token mint itself is answered directly here since callers don't
// care about it.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	_, pemKey := testKeyPEM(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"` + exp + `"}`))
	})
	mux.HandleFunc("/", handler)
	srv := httptest.NewServer(mux)

	app, err := NewApp("Iv1.abc", pemKey, srv.URL+"/api/v3")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(app, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestPermission(t *testing.T) {
	respond := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/collaborators/alice/permission") {
				t.Errorf("path = %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}

	cases := []struct {
		name string
		body string
		want forge.Permission
	}{
		{"admin permission", `{"permission":"admin"}`, forge.PermissionAdmin},
		{"write permission", `{"permission":"write"}`, forge.PermissionWrite},
		{"read permission", `{"permission":"read"}`, forge.PermissionRead},
		{"none permission", `{"permission":"none"}`, forge.PermissionNone},
		{"maintain role_name overrides write permission", `{"permission":"write","role_name":"maintain"}`, forge.PermissionMaintain},
		{"triage role_name overrides read permission", `{"permission":"read","role_name":"triage"}`, forge.PermissionTriage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := newTestClient(t, respond(tc.body))
			defer srv.Close()
			got, err := c.Permission(t.Context(), "acme", "widgets", "alice")
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Permission = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("unrecognized permission is an error", func(t *testing.T) {
		srv, c := newTestClient(t, respond(`{"permission":"bogus"}`))
		defer srv.Close()
		if _, err := c.Permission(t.Context(), "acme", "widgets", "alice"); err == nil {
			t.Fatal("expected an error for an unrecognized permission string")
		}
	})
}
