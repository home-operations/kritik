package webapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestMergeFailure(t *testing.T) {
	candidate := &configfile.Tenant{Slug: "alpha", Connections: []configfile.Connection{{Name: "own"}, {Name: "shared"}}}
	broken := errors.New("still broken")
	tests := []struct {
		name     string
		err      error
		baseline error
		status   int
		code     ErrorCode
		path     string
		message  string
	}{
		{
			name:   "the edited tenant is blamed with its prefix stripped",
			err:    &configfile.MergeError{Slug: "alpha", Err: errors.New(`configfile: dashboard[alpha].connections[0].app.clientId: "x" is not allowed`)},
			status: 422, code: CodeInvalidSpec, path: "connections[0].app.clientId", message: `connections[0].app.clientId: "x" is not allowed`,
		},
		{
			name:   "a path ending at a space",
			err:    &configfile.MergeError{Slug: "alpha", Err: errors.New(`configfile: dashboard[alpha].slug "X" must be lowercase`)},
			status: 422, code: CodeInvalidSpec, path: "slug", message: `slug "X" must be lowercase`,
		},
		{
			name:   "the whole tenant",
			err:    &configfile.MergeError{Slug: "alpha", Err: errors.New(`configfile: dashboard[alpha] (alpha) must list at least one connection`)},
			status: 422, code: CodeInvalidSpec, path: "", message: `(alpha) must list at least one connection`,
		},
		{
			name: "another tenant's slug is not revealed",
			err: &configfile.MergeError{Slug: "alpha", Err: errors.New(
				`configfile: dashboard[alpha].connections[0].name "x" duplicates a connection in tenant "secret-co"; names must be unique`)},
			status: 422, code: CodeInvalidSpec, path: "connections[0].name",
			message: `connections[0].name "x" duplicates a connection in another tenant; names must be unique`,
		},
		{
			name: "a duplicate slug names no tenant",
			err: &configfile.MergeError{Slug: "alpha", Err: errors.New(
				`configfile: dashboard[alpha].slug "alpha" duplicates tenants[3]`)},
			status: 422, code: CodeInvalidSpec, path: "slug", message: `slug "alpha" duplicates another tenant`,
		},
		{
			name: "a clash reported against a later tenant is the candidate's",
			err: &configfile.MergeError{Slug: "beta", Err: errors.New(
				`configfile: dashboard[beta].connections[0].name "shared" duplicates a connection in tenant "alpha"; names must be unique`)},
			status: 422, code: CodeInvalidSpec, path: "connections[1].name",
			message: `connections[1].name: conflicts with another tenant: "shared" duplicates a connection in tenant "alpha"; ` +
				`names must be unique`,
		},
		{
			name:     "another tenant blocks the write",
			err:      &configfile.MergeError{Slug: "beta", Err: errors.New(`configfile: dashboard[beta].connections[0].app.privateKey: cannot open`)},
			baseline: broken,
			status:   409, code: CodeConfigBlocked,
		},
		{
			name:     "the file itself",
			err:      errors.New("configfile: tenants must list at least one tenant"),
			baseline: broken,
			status:   409, code: CodeConfigBlocked,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, ok := errors.AsType[*apiError](mergeFailure("alpha", candidate, tt.err, func() error { return tt.baseline }))
			if !ok {
				t.Fatal("not an apiError")
			}
			if e.status != tt.status || e.code != tt.code {
				t.Fatalf("got %d %s, want %d %s", e.status, e.code, tt.status, tt.code)
			}
			if tt.status != 422 {
				return
			}
			if e.message != tt.message {
				t.Errorf("message = %q, want %q", e.message, tt.message)
			}
			if strings.Contains(e.message, "beta") || strings.Contains(e.message, "secret-co") {
				t.Errorf("message names another tenant: %q", e.message)
			}
			var d pathDetails
			if err := json.Unmarshal(e.details, &d); err != nil || d.Path != tt.path {
				t.Errorf("details = %s, want path %q", e.details, tt.path)
			}
		})
	}
}

func TestDecodeFailure(t *testing.T) {
	tests := []struct {
		spec, path, message string
	}{
		{`{"slug":"alpha","nope":1}`, "nope", "tenant spec: field nope not found"},
		{`{"slug":"alpha","connections":[{"name":"a","bogus":true}]}`, "bogus", "tenant spec: field bogus not found"},
		{`{"slug":"beta"}`, "slug", `tenant spec slug "beta" does not match "alpha"`},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			_, err := configfile.DecodeTenant(configfile.DashboardTenant{Slug: "alpha", Spec: json.RawMessage(tt.spec)})
			if err == nil {
				t.Fatal("decoded")
			}
			e, _ := errors.AsType[*apiError](decodeFailure(err))
			var d pathDetails
			_ = json.Unmarshal(e.details, &d)
			if e.status != 422 || e.message != tt.message || d.Path != tt.path || strings.Contains(e.message, "line ") {
				t.Errorf("got %d %q path %q, want %q path %q (from %v)", e.status, e.message, d.Path, tt.message, tt.path, err)
			}
		})
	}
}
