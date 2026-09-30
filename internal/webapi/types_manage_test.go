package webapi

import (
	"encoding/json"
	"maps"

	"github.com/home-operations/kritik/internal/configfile"
)

var goldenUser = User{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com", AvatarURL: "https://img.example/a.png"}

func init() {
	maps.Copy(goldens, map[string]any{
		"meta": Meta{
			Version: "v1.2.3", Management: true, WebURL: "https://kritik.example",
		},
		"instance_config": InstanceConfig{
			Revision: 3, Editable: true,
			Inherited: InstanceInherited{
				Providers: map[string]InheritedProvider{"openrouter": {Type: configfile.ProviderOpenRouter, Source: configfile.SourceEnv}},
				Review:    &InheritedValue{Value: "openrouter/acme-large", Source: configfile.SourceFile},
				Embedding: &InheritedEmbedding{BaseURL: "https://openrouter.ai/api/v1", Model: "acme-embed", Dims: 1024, Source: configfile.SourceFile},
			},
			Spec: json.RawMessage(`{"connections":[{"name":"alpha-bot","forge":"github","accounts":["alpha"],` +
				`"app":{"clientId":"Iv1.alpha","privateKey":{"set":true},"webhookSecret":{"set":true}}}]}`),
		},
		"account_config": AccountConfig{
			Revision: 3, Editable: true,
			Inherited: Inherited{
				Account: goldenRepoSettings, AccountSources: map[string]configfile.Source{"models.review": configfile.SourceDefaults},
				Repository:        goldenRepoSettings,
				RepositorySources: map[string]configfile.Source{"models.review": configfile.SourceDefaults, "mode": configfile.SourceAccount},
			},
			Spec: json.RawMessage(`{"forge":"github","name":"alpha","providers":{"own":{"type":"openai","apiKey":{"set":true}}}}`),
		},
		"update_config_request": UpdateConfigRequest{Revision: 3, Spec: json.RawMessage(`{"forge":"github","name":"alpha"}`), ConfirmReindex: true},
		"turn_on_request":       TurnOnRequest{On: true},
		"config_write_result": ConfigWriteResult{
			Revision: 4, Generated: map[string]string{"connections[alpha-bot].app.webhookSecret": "00ff"},
		},
		"accepted": Accepted{JobID: 42},
		"app_installation": AppInstallation{
			ID: 2, Account: "stranger", AccountType: "User", Served: false,
			URL: "https://github.com/settings/installations/2",
		},
		"setup_status": SetupStatus{
			WebURL: "https://kritik.example", HooksURL: "https://kritik.example/hooks/",
			FileConnections: []string{"alpha-bot"}, Connections: []string{"alpha-bot"}, ReviewModel: "openrouter/acme-large",
		},
		"provider_test_request": ProviderTestRequest{
			Type: configfile.ProviderOpenRouter, APIKey: json.RawMessage(`{"keep":true}`), Name: "own", Account: "github/alpha",
		},
		"embedding_test_request": EmbeddingTestRequest{
			BaseURL: "https://openrouter.ai/api/v1", Model: "voyage-code-3", Dims: 1024, APIKey: json.RawMessage(`{"value":"sk"}`),
		},
		"test_result": TestResult{OK: true, Models: []string{"acme-large", "acme-small"}},
		"account_repositories": AccountRepositories{
			Account: "alpha", Installed: true,
			Repositories: []AppRepository{{Name: "one", FullName: "alpha/one", DefaultBranch: "main"}},
		},
		"register_result":      RegisterResult{Added: 3},
		"app_manifest_request": AppManifestRequest{Connection: "beta-bot", Organization: "beta", Name: "kritik-beta", Public: true},
		"app_manifest_form": AppManifestForm{
			URL:      "https://github.com/organizations/beta/settings/apps/new?state=s1",
			Manifest: json.RawMessage(`{"name":"kritik-beta","url":"https://kritik.example"}`),
		},
		"app_manifest_result": AppManifestResult{
			Connection: "beta-bot", Slug: "kritik-beta", InstallURL: "https://github.com/apps/kritik-beta/installations/new",
			ClientID: "Iv1.beta", ClientSecret: "cs-1",
		},
		"audit_event": AuditEvent{
			ID: "7", At: t0, Actor: &goldenUser, Account: "github/alpha", Action: AuditAccountUpdate, Target: "github/alpha",
			Detail: json.RawMessage(`{"revision":2,"secretsChanged":["accounts[github/alpha].providers.own.apiKey"]}`),
		},
	})
}
