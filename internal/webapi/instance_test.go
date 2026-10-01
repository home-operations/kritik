package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/home-operations/kritika/internal/auth"
	"github.com/home-operations/kritika/internal/config"
	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/configfile/configfiletest"
)

func TestInstanceSettings(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-secret")
	f := configfiletest.Load(t, `providers:
  gw: { type: openai, baseUrl: "https://kritika:hunter2@gw.example/v1", apiKey: { env: TEST_KEY } }
apps:
  - { name: acme-bot, accounts: [acme, org-1], clientId: Iv1.acme, privateKey: { env: TEST_KEY }, webhookSecret: { env: TEST_KEY } }
`)
	env := []config.EnvVar{
		{Name: "KRITIKA_ADDR", Value: ":9090", Set: true},
		{Name: "KRITIKA_METRICS_ADDR", Value: ":8081"},
		{Name: "KRITIKA_GATEWAY_URL", Value: "https://user:pass@gw.example", Set: true},
		{Name: "KRITIKA_DATABASE_URL", Value: "set", Secret: true, Set: true},
	}
	rows := map[string]InstanceSetting{}
	for _, s := range instanceSettings(f, env) {
		rows[s.Section+" "+s.Key] = s
	}
	for key, want := range map[string]InstanceSetting{
		"environment KRITIKA_ADDR":         {"environment", "KRITIKA_ADDR", ":9090", configfile.SourceEnv},
		"environment KRITIKA_METRICS_ADDR": {"environment", "KRITIKA_METRICS_ADDR", ":8081", configfile.SourceDefault},
		"environment KRITIKA_GATEWAY_URL": {
			"environment", "KRITIKA_GATEWAY_URL", "https://gw.example (credentials hidden)", configfile.SourceEnv,
		},
		"environment KRITIKA_DATABASE_URL": {"environment", "KRITIKA_DATABASE_URL", "set", configfile.SourceEnv},
		"apps acme-bot":                    {"apps", "acme-bot", "acme, org-1, webhook /hooks/acme-bot", configfile.SourceFile},
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
// and without their keys.
func TestInstanceSettingsFileLayer(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-secret")
	t.Setenv("KRITIKA_DEFAULTS_MODELS_REVIEW", "gw/big")
	f, err := configfile.Parse([]byte(`providers:
  gw: { type: openai, baseUrl: "https://kritika:hunter2@gw.example/v1", apiKey: { env: TEST_KEY } }
defaults: { models: { fallback: gw/small }, forks: true }
embedding: { model: gw/e1, dims: 8 }
`))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]InstanceSetting{}
	for _, s := range instanceSettings(f, nil) {
		rows[s.Section+" "+s.Key] = s
	}
	for key, want := range map[string]InstanceSetting{
		"providers gw":             {"providers", "gw", "openai at https://gw.example/v1 (credentials hidden)", configfile.SourceFile},
		"defaults models.review":   {"defaults", "models.review", "gw/big", configfile.SourceEnv},
		"defaults models.fallback": {"defaults", "models.fallback", "gw/small", configfile.SourceFile},
		"defaults forks":           {"defaults", "forks", "true", configfile.SourceFile},
		"embedding gw/e1":          {"embedding", "gw/e1", "8 dimensions", configfile.SourceFile},
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
	ts := newTestServer(t, "https://kritika.example")
	ts.srv.env = []config.EnvVar{{Name: "KRITIKA_ADDR", Value: ":8080"}}
	get := func(p *auth.Principal) *httptest.ResponseRecorder {
		return ts.as(p, httptest.NewRequest("GET", "/api/v1/admin/instance", nil))
	}
	if w := get(memberOf(t, ts.file, "alpha")); w.Code != http.StatusNotFound {
		t.Fatalf("account admin: status = %d, want 404", w.Code)
	}
	w := get(&auth.Principal{Admin: true})
	var rows []InstanceSetting
	if err := json.Unmarshal(w.Body.Bytes(), &rows); w.Code != http.StatusOK || err != nil || rows[0].Key != "KRITIKA_ADDR" {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
}
