package worker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
)

func TestRefuseRetries(t *testing.T) {
	for status, retry := range map[int]bool{
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          false,
		http.StatusTooManyRequests:     false,
		http.StatusUnauthorized:        false,
		http.StatusBadRequest:          false,
	} {
		rec := httptest.NewRecorder()
		refuse(rec, status, "code", "message")
		if got := rec.Header().Get("X-Should-Retry") != "false"; got != retry || rec.Code != status {
			t.Fatalf("%d: retry = %v, want %v", status, got, retry)
		}
	}
}

func TestMaskProvider(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-provider")
	f, err := configfile.Parse([]byte(`providers:
  p:
    type: openai
    baseUrl: https://kritik:url-secret@llm.example/v1
    apiKey: { env: TEST_PROVIDER_KEY }
` + minimalGatewayFile))
	if err != nil {
		t.Fatal(err)
	}
	got := maskProvider(`POST "https://kritik:url-secret@llm.example/v1/chat/completions": 401 {"error":"bad key sk-provider"}`, f.Providers["p"])
	want := `POST "https://***@llm.example/v1/chat/completions": 401 {"error":"bad key ***"}`
	if got != want {
		t.Fatalf("masked = %s\nwant     %s", got, want)
	}
}

// minimalGatewayFile is the rest of a configuration file the provider
// above sits in.
const minimalGatewayFile = `tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: forgejo
        host: forge.example.com
        account: acme
        token: { env: TEST_PROVIDER_KEY }
        webhookSecret: { env: TEST_PROVIDER_KEY }
`
