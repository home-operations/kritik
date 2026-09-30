package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
)

func TestSetupStatus(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	const conn = `connections:
  - { name: acme-bot, forge: github, accounts: [acme], app: { clientId: Iv1.acme, privateKey: { env: TEST_KEY }, webhookSecret: { env: TEST_KEY } } }
`
	const provider = "providers:\n  p: { type: openai, apiKey: { env: TEST_KEY } }\n"
	base := SetupStatus{WebURL: "https://kritik.example", HooksURL: "https://kritik.example/hooks/", Connections: []string{}}
	with := func(edit func(*SetupStatus)) SetupStatus {
		s := base
		edit(&s)
		return s
	}
	for _, tt := range []struct {
		name, doc string
		want      SetupStatus
	}{
		{"a fresh instance", "", base},
		{"a connection alone", conn, with(func(s *SetupStatus) {
			s.Connections = []string{"acme-bot"}
		})},
		{"ready", conn + "defaults: { models: { review: p/x } }\n" + provider, with(func(s *SetupStatus) {
			s.Connections, s.ReviewModel = []string{"acme-bot"}, "p/x"
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := setupStatus(configfiletest.Load(t, tt.doc), "https://kritik.example/")
			if got.WebURL != tt.want.WebURL || got.HooksURL != tt.want.HooksURL || !slices.Equal(got.Connections, tt.want.Connections) || got.ReviewModel != tt.want.ReviewModel ||
				got.Embedding != tt.want.Embedding {
				t.Fatalf("setupStatus = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSetupReportsARefusedReload: the status says why the file's latest
// content was refused, so the Configuration page can.
func TestSetupReportsARefusedReload(t *testing.T) {
	ts := newTestServer(t, "https://kritik.example")
	ts.srv.configError = func() error { return errors.New("configfile: polling.interval must not be negative") }
	w := ts.as(&auth.Principal{Admin: true}, httptest.NewRequest("GET", "/api/v1/admin/setup", nil))
	var got SetupStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil ||
		got.ConfigError != "configfile: polling.interval must not be negative" {
		t.Fatalf("setup = %d %s", w.Code, w.Body)
	}
}
