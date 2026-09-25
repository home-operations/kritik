package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

func decodeSpec(t *testing.T, spec string) *configfile.Tenant {
	t.Helper()
	tn, err := configfile.DecodeTenant(configfile.DashboardTenant{Slug: "alpha", Spec: json.RawMessage(spec)})
	if err != nil {
		t.Fatalf("DecodeTenant(%s): %v", spec, err)
	}
	return &tn
}

func TestOperatorOnlyChange(t *testing.T) {
	const base = `"slug":"alpha","installations":[{"name":"a","forge":"forgejo","account":"alpha"}]`
	tests := []struct {
		name     string
		old, new string
		want     string
	}{
		{name: "nothing operator-only changes", old: `{` + base + `,"filter":"true"}`, new: `{` + base + `,"filter":"false"}`},
		{name: "limits set", old: `{` + base + `}`, new: `{` + base + `,"limits":{"concurrency":9}}`, want: "limits"},
		{name: "limits changed", old: `{` + base + `,"limits":{"reviewsPerDay":1}}`, new: `{` + base + `,"limits":{"reviewsPerDay":2}}`, want: "limits"},
		{name: "limits unchanged", old: `{` + base + `,"limits":{"reviewsPerDay":1}}`, new: `{` + base + `,"limits":{"reviewsPerDay":1}}`},
		{name: "runner added", old: `{` + base + `}`, new: `{` + base + `,"runner":{"activeDeadlineSeconds":60}}`, want: "runner"},
		{name: "empty runner is no runner", old: `{` + base + `}`, new: `{` + base + `,"runner":{}}`},
		{
			name: "runner resources changed",
			old:  `{` + base + `,"runner":{"resources":{"limits":{"cpu":"1"}}}}`,
			new:  `{` + base + `,"runner":{"resources":{"limits":{"cpu":"2"}}}}`, want: "runner",
		},
		{
			name: "repository mode set on a new repository",
			old:  `{` + base + `}`, new: `{` + base + `,"repositories":[{"name":"alpha/x","mode":"agentic"}]}`,
			want: "repositories[0].mode",
		},
		{
			name: "single is the unset mode",
			old:  `{` + base + `,"repositories":[{"name":"alpha/x"}]}`, new: `{` + base + `,"repositories":[{"name":"alpha/x","mode":"single"}]}`,
		},
		{
			name: "agent changed on a reordered repository",
			old:  `{` + base + `,"repositories":[{"name":"alpha/x","agent":{"maxSteps":5}},{"name":"alpha/y"}]}`,
			new:  `{` + base + `,"repositories":[{"name":"alpha/y"},{"name":"alpha/x","agent":{"maxSteps":6}}]}`,
			want: "repositories[1].agent",
		},
		{
			name: "agent commands changed",
			old:  `{` + base + `,"repositories":[{"name":"alpha/x","agent":{"commands":["rg"]}}]}`,
			new:  `{` + base + `,"repositories":[{"name":"alpha/x","agent":{"commands":["rg","curl"]}}]}`,
			want: "repositories[0].agent",
		},
		{
			name: "other repository fields are the admin's",
			old:  `{` + base + `,"repositories":[{"name":"alpha/x","agent":{"maxSteps":5},"mode":"agentic"}]}`,
			new:  `{` + base + `,"repositories":[{"name":"alpha/x","agent":{"maxSteps":5},"mode":"agentic","ignore":["a/**"]}]}`,
		},
		{
			name: "removing a repository with agent settings resets them",
			old:  `{` + base + `,"repositories":[{"name":"alpha/x","agent":{"maxSteps":5}}]}`, new: `{` + base + `}`,
			want: "repositories",
		},
		{
			name: "removing a plain repository is fine",
			old:  `{` + base + `,"repositories":[{"name":"alpha/x"}]}`, new: `{` + base + `}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := operatorOnlyChange(decodeSpec(t, tt.old), decodeSpec(t, tt.new))
			if got != tt.want {
				t.Errorf("operatorOnlyChange = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeFailure(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		code    ErrorCode
		path    string
		message string
	}{
		{
			name:   "the edited tenant is blamed with its prefix stripped",
			err:    &configfile.MergeError{Slug: "alpha", Err: errors.New(`configfile: dashboard[alpha].installations[0].host: "x" is not allowed`)},
			status: 422, code: CodeInvalidSpec, path: "installations[0].host", message: `installations[0].host: "x" is not allowed`,
		},
		{
			name:   "a path ending at a space",
			err:    &configfile.MergeError{Slug: "alpha", Err: errors.New(`configfile: dashboard[alpha].slug "X" must be lowercase`)},
			status: 422, code: CodeInvalidSpec, path: "slug", message: `slug "X" must be lowercase`,
		},
		{
			name:   "the whole tenant",
			err:    &configfile.MergeError{Slug: "alpha", Err: errors.New(`configfile: dashboard[alpha] (alpha) must list at least one installation`)},
			status: 422, code: CodeInvalidSpec, path: "", message: `(alpha) must list at least one installation`,
		},
		{
			name: "a decode error",
			err: &configfile.MergeError{Slug: "alpha", Err: fmt.Errorf("configfile: tenant spec: %w",
				errors.New("yaml: field nope not found"))},
			status: 422, code: CodeInvalidSpec, path: "", message: "tenant spec: yaml: field nope not found",
		},
		{
			name:   "another tenant blocks the write",
			err:    &configfile.MergeError{Slug: "beta", Err: errors.New(`configfile: dashboard[beta].installations[0].token: cannot open`)},
			status: 409, code: CodeConfigBlocked,
		},
		{
			name:   "the file itself",
			err:    errors.New("configfile: tenants must list at least one tenant"),
			status: 409, code: CodeConfigBlocked,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, ok := errors.AsType[*apiError](mergeFailure("alpha", tt.err))
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
			var d pathDetails
			if err := json.Unmarshal(e.details, &d); err != nil || d.Path != tt.path {
				t.Errorf("details = %s, want path %q", e.details, tt.path)
			}
		})
	}
}

func TestLeavesAdmin(t *testing.T) {
	web := configfile.Web{
		SignIn:    []configfile.SignIn{{Name: "corp", Type: configfile.SignInOIDC}},
		Operators: []string{"corp:op-sub"},
	}
	admin := []store.MemberGrant{{Source: store.SourceInvite, Role: store.RoleAdmin}}
	forgeAdmin := []store.MemberGrant{{Source: store.SourceForge, Role: store.RoleAdmin}, {Source: store.SourceInvite, Role: store.RoleAdmin}}
	otherAdmin := []store.AdminIdentity{{AccountID: "b", Identity: store.SignInIdentity{Provider: "corp", Subject: "b-sub"}}}
	operatorAdmin := []store.AdminIdentity{{AccountID: "op", Identity: store.SignInIdentity{Provider: "corp", Subject: "op-sub"}}}
	tests := []struct {
		name   string
		grants []store.MemberGrant
		after  auth.Role // "" removes the invite grant
		others []store.AdminIdentity
		want   bool
	}{
		{name: "self-removal leaving no admin", grants: admin, after: "", want: true},
		{name: "self-demotion leaving no admin", grants: admin, after: store.RoleMember, want: true},
		{name: "another admin remains", grants: admin, after: "", others: otherAdmin},
		{name: "operators do not count", grants: admin, after: "", others: operatorAdmin, want: true},
		{name: "a forge admin grant keeps the account admin", grants: forgeAdmin, after: ""},
		{name: "staying admin", grants: admin, after: store.RoleAdmin},
		{name: "a member was never the last admin", grants: []store.MemberGrant{{Source: store.SourceInvite, Role: store.RoleMember}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := leavesNoAdmin(web, tt.grants, tt.after, tt.others); got != tt.want {
				t.Errorf("leavesNoAdmin = %v, want %v", got, tt.want)
			}
		})
	}
}
