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
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
)

func TestInstanceSettings(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-secret")
	f := configfiletest.Load(t, `providers:
  gw: { type: openai, baseUrl: "https://kritik:hunter2@gw.example/v1", apiKey: { env: TEST_KEY } }
polling: { interval: 2m }
connections:
  - { name: acme-bot, forge: github, accounts: [acme, org-1], app: { clientId: Iv1.acme, privateKey: { env: TEST_KEY }, webhookSecret: { env: TEST_KEY } } }
`)
	env := []config.EnvVar{
		{Name: "KRITIK_ADDR", Value: ":9090", Set: true},
		{Name: "KRITIK_METRICS_ADDR", Value: ":8081"},
		{Name: "KRITIK_GATEWAY_URL", Value: "https://user:pass@gw.example", Set: true},
		{Name: "KRITIK_DATABASE_URL", Value: "set", Secret: true, Set: true},
	}
	rows := map[string]InstanceSetting{}
	for _, s := range instanceSettings(f, env) {
		rows[s.Section+" "+s.Key] = s
	}
	for key, want := range map[string]InstanceSetting{
		"environment KRITIK_ADDR":         {"environment", "KRITIK_ADDR", ":9090", configfile.SourceEnv},
		"environment KRITIK_METRICS_ADDR": {"environment", "KRITIK_METRICS_ADDR", ":8081", configfile.SourceDefault},
		"environment KRITIK_GATEWAY_URL": {
			"environment", "KRITIK_GATEWAY_URL", "https://gw.example (credentials hidden)", configfile.SourceEnv,
		},
		"environment KRITIK_DATABASE_URL": {"environment", "KRITIK_DATABASE_URL", "set", configfile.SourceEnv},
		"connections acme-bot":            {"connections", "acme-bot", "acme, org-1, webhook /hooks/acme-bot", configfile.SourceFile},
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

// TestInstanceSettingsFileLayer: the providers, default models and
// embedder the file and the environment set are listed with their source,
// whatever the spec lays over them, and without their keys.
func TestInstanceSettingsFileLayer(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-secret")
	t.Setenv("KRITIK_DEFAULTS_MODELS_REVIEW", "gw/big")
	base, err := configfile.Parse([]byte(`providers:
  gw: { type: openai, baseUrl: "https://kritik:hunter2@gw.example/v1", apiKey: { env: TEST_KEY } }
defaults: { models: { fallback: gw/small }, mode: agentic }
embedding: { baseUrl: https://embed.example/v1, apiKey: { env: TEST_KEY }, model: e1, dims: 8 }
`))
	if err != nil {
		t.Fatal(err)
	}
	f, err := configfile.Merge(base, configfile.InstanceSpec{Spec: json.RawMessage(`{"defaults":{"models":{"review":"gw/huge"}}}`), Revision: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]InstanceSetting{}
	for _, s := range instanceSettings(f, nil) {
		rows[s.Section+" "+s.Key] = s
	}
	for key, want := range map[string]InstanceSetting{
		"providers gw":             {"providers", "gw", "openai at https://gw.example/v1 (credentials hidden)", configfile.SourceFile},
		"defaults models.review":   {"defaults", "models.review", "gw/big, overridden by the dashboard", configfile.SourceEnv},
		"defaults models.fallback": {"defaults", "models.fallback", "gw/small", configfile.SourceFile},
		"defaults mode":            {"defaults", "mode", "agentic", configfile.SourceFile},
		"embedding e1":             {"embedding", "e1", "8 dimensions at https://embed.example/v1", configfile.SourceFile},
	} {
		if rows[key] != want {
			t.Errorf("%s = %+v, want %+v", key, rows[key], want)
		}
	}
	for _, s := range rows {
		if strings.Contains(s.Value, "hunter2") || strings.Contains(s.Value, "sk-secret") {
			t.Fatalf("%s %s shows a secret: %q", s.Section, s.Key, s.Value)
		}
	}
}

func TestInstanceSettingsAreAdminOnly(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	ts.srv.env = []config.EnvVar{{Name: "KRITIK_ADDR", Value: ":8080"}}
	get := func(p *auth.Principal) *httptest.ResponseRecorder {
		return ts.as(p, httptest.NewRequest("GET", "/api/v1/admin/instance", nil))
	}
	if w := get(memberOf(t, ts.file, "alpha")); w.Code != http.StatusNotFound {
		t.Fatalf("account admin: status = %d, want 404", w.Code)
	}
	w := get(&auth.Principal{Admin: true})
	var rows []InstanceSetting
	if err := json.Unmarshal(w.Body.Bytes(), &rows); w.Code != http.StatusOK || err != nil || rows[0].Key != "KRITIK_ADDR" {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
}
