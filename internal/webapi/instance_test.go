package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
)

func TestInstanceSettings(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-secret")
	f, err := configfile.Parse([]byte(`providers:
  gw: { type: openai, baseUrl: "https://kritik:hunter2@gw.example/v1", apiKey: { env: TEST_KEY } }
polling: { interval: 2m }
tenants:
  - slug: acme
    connections:
      - { name: acme-bot, forge: github, accounts: [acme], app: { clientId: Iv1.acme, privateKey: { env: TEST_KEY }, webhookSecret: { env: TEST_KEY } } }
`))
	if err != nil {
		t.Fatal(err)
	}
	env := []config.EnvVar{
		{Name: "KRITIK_ADDR", Value: ":9090", Set: true},
		{Name: "KRITIK_METRICS_ADDR", Value: ":8081"},
		{Name: "KRITIK_EMBED_BASE_URL", Value: "https://user:pass@embed.example", Set: true},
		{Name: "KRITIK_DATABASE_URL", Value: "set", Secret: true, Set: true},
	}
	rows := map[string]InstanceSetting{}
	for _, s := range instanceSettings(f, env) {
		rows[s.Section+" "+s.Key] = s
	}
	for key, want := range map[string]InstanceSetting{
		"environment KRITIK_ADDR":         {"environment", "KRITIK_ADDR", ":9090", configfile.SourceEnv},
		"environment KRITIK_METRICS_ADDR": {"environment", "KRITIK_METRICS_ADDR", ":8081", configfile.SourceDefault},
		"environment KRITIK_EMBED_BASE_URL": {
			"environment", "KRITIK_EMBED_BASE_URL", "https://embed.example (credentials hidden)", configfile.SourceEnv,
		},
		"environment KRITIK_DATABASE_URL": {"environment", "KRITIK_DATABASE_URL", "set", configfile.SourceEnv},
		"providers gw":                    {"providers", "gw", "openai at https://gw.example/v1 (credentials hidden)", configfile.SourceFile},
		"polling interval":                {"polling", "interval", "2m0s", configfile.SourceFile},
		"polling lookback":                {"polling", "lookback", "24h0m0s", configfile.SourceDefault},
	} {
		if rows[key] != want {
			t.Errorf("%s = %+v, want %+v", key, rows[key], want)
		}
	}
	for _, s := range rows {
		if strings.Contains(s.Value, "hunter2") || strings.Contains(s.Value, "sk-secret") || strings.Contains(s.Value, "pass@") {
			t.Fatalf("%s %s shows a secret: %q", s.Section, s.Key, s.Value)
		}
	}
}

func TestInstanceSettingsAreOperatorOnly(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	ts.srv.env = []config.EnvVar{{Name: "KRITIK_ADDR", Value: ":8080"}}
	get := func(p *auth.Principal) *httptest.ResponseRecorder {
		return ts.as(p, httptest.NewRequest("GET", "/api/v1/operator/instance", nil))
	}
	if w := get(memberOf(t, ts.file, "alpha")); w.Code != http.StatusNotFound {
		t.Fatalf("tenant admin: status = %d, want 404", w.Code)
	}
	w := get(&auth.Principal{Operator: true})
	var rows []InstanceSetting
	if err := json.Unmarshal(w.Body.Bytes(), &rows); w.Code != http.StatusOK || err != nil || rows[0].Key != "KRITIK_ADDR" {
		t.Fatalf("operator: %d %s", w.Code, w.Body)
	}
}
