package webapi

import (
	"encoding/json"
	"maps"
)

var goldenUser = User{ID: "acct-1", DisplayName: "Ada", Email: "ada@example.com", AvatarURL: "https://img.example/a.png"}

func init() {
	maps.Copy(goldens, map[string]any{
		"meta":            Meta{Version: "v1.2.3", WebURL: "https://kritik.example"},
		"turn_on_request": TurnOnRequest{On: true},
		"accepted":        Accepted{JobID: 42},
		"app_installation": AppInstallation{
			ID: 2, Account: "stranger", AccountType: "User", Served: false,
			URL: "https://github.com/settings/installations/2",
		},
		"setup_status": SetupStatus{
			WebURL: "https://kritik.example", HooksURL: "https://kritik.example/hooks/", Connections: []string{"alpha-bot"},
			ReviewModel: "openrouter/acme-large",
		},
		"register_result": RegisterResult{Added: 3},
		"audit_event": AuditEvent{
			ID: "7", At: t0, Actor: &goldenUser, Account: "github/alpha", Action: AuditRepoReindex, Target: "alpha/one",
			Detail: json.RawMessage(`{"jobId":42}`),
		},
	})
}
