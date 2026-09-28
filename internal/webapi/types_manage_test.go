package webapi

import (
	"encoding/json"
	"maps"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
)

var goldenUser = User{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com", AvatarURL: "https://img.example/a.png"}

func init() {
	maps.Copy(goldens, map[string]any{
		"meta": Meta{
			Version: "v1.2.3", Management: true, WebURL: "https://kritik.example",
			SignIn: []auth.ProviderInfo{{Name: "corp", Type: configfile.SignInOIDC, DisplayName: "Corp"}},
		},
		"account_config": AccountConfig{
			ManagedBy: configfile.OriginDashboard, Revision: new(int64(3)), Editable: true, Policy: fieldPolicies(true),
			Inherited: Inherited{
				Account: goldenRepoSettings, AccountSources: map[string]configfile.Source{"models.review": configfile.SourceFile},
				Repository:        goldenRepoSettings,
				RepositorySources: map[string]configfile.Source{"models.review": configfile.SourceFile, "mode": configfile.SourceDashboard},
			},
			Spec: json.RawMessage(`{"slug":"alpha","connections":[{"name":"alpha-bot","app":{"privateKey":{"set":true}}}]}`),
		},
		"create_account_request": CreateAccountRequest{Slug: "alpha", Spec: json.RawMessage(`{"slug":"alpha"}`)},
		"update_account_request": UpdateAccountRequest{Revision: 3, Spec: json.RawMessage(`{"slug":"alpha"}`)},
		"account_write_result": AccountWriteResult{
			Slug: "alpha", Revision: 1, Generated: map[string]string{"connections[alpha-bot].app.webhookSecret": "00ff"},
		},
		"accepted": Accepted{JobID: 42},
		"audit_event": AuditEvent{
			ID: "7", At: t0, Actor: &goldenUser, Account: "alpha", Action: AuditAccountUpdate, Target: "alpha",
			Detail: json.RawMessage(`{"revision":2,"secretsChanged":["connections[alpha-bot].app.privateKey"]}`),
		},
	})
}
