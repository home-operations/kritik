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
		"instance_config": InstanceConfig{
			Revision: 3, Editable: true,
			Spec: json.RawMessage(`{"connections":[{"name":"alpha-bot","forge":"github","accounts":["alpha"],` +
				`"app":{"clientId":"Iv1.alpha","privateKey":{"set":true},"webhookSecret":{"set":true}}}]}`),
		},
		"account_config": AccountConfig{
			Revision: 3, Editable: true, Policy: fieldPolicies(true),
			Inherited: Inherited{
				Account: goldenRepoSettings, AccountSources: map[string]configfile.Source{"models.review": configfile.SourceDefaults},
				Repository:        goldenRepoSettings,
				RepositorySources: map[string]configfile.Source{"models.review": configfile.SourceDefaults, "mode": configfile.SourceAccount},
			},
			Spec: json.RawMessage(`{"forge":"github","name":"alpha","providers":{"own":{"type":"openai","apiKey":{"set":true}}}}`),
		},
		"update_config_request": UpdateConfigRequest{Revision: 3, Spec: json.RawMessage(`{"forge":"github","name":"alpha"}`), ConfirmReindex: true},
		"config_write_result": ConfigWriteResult{
			Revision: 4, Generated: map[string]string{"connections[alpha-bot].app.webhookSecret": "00ff"},
		},
		"accepted": Accepted{JobID: 42},
		"audit_event": AuditEvent{
			ID: "7", At: t0, Actor: &goldenUser, Account: "github/alpha", Action: AuditAccountUpdate, Target: "github/alpha",
			Detail: json.RawMessage(`{"revision":2,"secretsChanged":["accounts[github/alpha].providers.own.apiKey"]}`),
		},
	})
}
