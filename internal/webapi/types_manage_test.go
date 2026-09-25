package webapi

import (
	"encoding/json"
	"maps"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

var goldenAccount = Account{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com", AvatarURL: "https://img.example/a.png"}

func init() {
	maps.Copy(goldens, map[string]any{
		"meta": Meta{
			Version: "v1.2.3", Management: true, WebURL: "https://kritik.example",
			SignIn: []auth.ProviderInfo{{Name: "corp", Type: configfile.SignInOIDC, DisplayName: "Corp"}},
		},
		"tenant_config": TenantConfig{
			ManagedBy: configfile.OriginDashboard, Revision: new(int64(3)), Editable: true, OperatorOnlyFields: operatorOnlyFields,
			Spec: json.RawMessage(`{"slug":"alpha","installations":[{"name":"alpha-bot","token":{"set":true}}]}`),
		},
		"create_tenant_request": CreateTenantRequest{Slug: "alpha", Spec: json.RawMessage(`{"slug":"alpha"}`)},
		"update_tenant_request": UpdateTenantRequest{Revision: 3, Spec: json.RawMessage(`{"slug":"alpha"}`)},
		"tenant_write_result": TenantWriteResult{
			Slug: "alpha", Revision: 1, Generated: map[string]string{"installations[alpha-bot].webhookSecret": "00ff"},
		},
		"members": Members{
			Members: []Member{{
				Account: goldenAccount, Role: auth.RoleAdmin,
				Sources: []MemberSource{{Source: store.SourceForge, Role: auth.RoleMember}, {Source: store.SourceInvite, Role: auth.RoleAdmin}},
			}},
			Invites: []Invite{{ID: "inv-1", Email: "bob@example.com", Role: auth.RoleMember, CreatedBy: &goldenAccount, ExpiresAt: t1}},
		},
		"create_invite_request": CreateInviteRequest{Email: "bob@example.com", Role: auth.RoleMember, TTLHours: new(24)},
		"update_member_request": UpdateMemberRequest{Role: auth.RoleAdmin},
		"member_removed":        MemberRemoved{Note: removedNote},
		"accepted":              Accepted{JobID: 42},
		"audit_event": AuditEvent{
			ID: "7", At: t0, Actor: &goldenAccount, Tenant: "alpha", Action: AuditTenantUpdate, Target: "alpha",
			Detail: json.RawMessage(`{"revision":2,"secretsChanged":["installations[alpha-bot].token"]}`),
		},
	})
}
