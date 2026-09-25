package worker

import (
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestTranscriptMask(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-provider")
	t.Setenv("TEST_EGRESS_TOKEN", "ghp-egress")
	f, err := configfile.Parse([]byte(`providers:
  p:
    type: openai
    baseUrl: https://kritik:url-secret@llm.example/v1
    apiKey: { env: TEST_PROVIDER_KEY }
egress:
  allowHosts: [api.example.com]
  credentials:
    api.example.com: { env: TEST_EGRESS_TOKEN }
` + minimalGatewayFile))
	if err != nil {
		t.Fatal(err)
	}
	mask := transcriptMask(f, f.Providers["p"], "krk_run", "")
	tests := map[string]string{
		"key sk-provider":                           "key ***",
		"https://kritik:url-secret@llm/":            "https://***@llm/",
		"auth Bearer ghp-egress or bare ghp-egress": "auth *** or bare ***",
		"token krk_run":                             "token ***",
		"nothing secret":                            "nothing secret",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := mask(in); got != want {
				t.Fatalf("mask(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
