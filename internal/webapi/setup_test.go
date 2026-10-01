package webapi

import (
	"slices"
	"testing"

	"github.com/home-operations/kritika/internal/configfile/configfiletest"
)

func TestSetupStatus(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	const conn = `apps:
  - { name: acme-bot, accounts: [acme], clientId: Iv1.acme, privateKey: { env: TEST_KEY }, webhookSecret: { env: TEST_KEY } }
`
	const provider = "providers:\n  p: { type: openai, apiKey: { env: TEST_KEY } }\n"
	base := SetupStatus{WebURL: "https://kritika.example", HooksURL: "https://kritika.example/hooks/", Connections: []string{}}
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
			got := setupStatus(configfiletest.Load(t, tt.doc), "https://kritika.example/")
			if got.WebURL != tt.want.WebURL || got.HooksURL != tt.want.HooksURL || !slices.Equal(got.Connections, tt.want.Connections) || got.ReviewModel != tt.want.ReviewModel ||
				got.Embedding != tt.want.Embedding {
				t.Fatalf("setupStatus = %+v, want %+v", got, tt.want)
			}
		})
	}
}
