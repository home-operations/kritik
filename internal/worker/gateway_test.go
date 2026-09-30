package worker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
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
	f, err := configfiletest.Parse(t, `providers:
  p:
    type: openai
    baseUrl: https://kritik:url-secret@llm.example/v1
    apiKey: { env: TEST_PROVIDER_KEY }
`+minimalGatewayFile)
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
const minimalGatewayFile = `apps:
  - name: acme-bot
    accounts: [acme]
    clientId: Iv1.test
    privateKey: { env: TEST_PROVIDER_KEY }
    webhookSecret: { env: TEST_PROVIDER_KEY }
`

func TestServedRef(t *testing.T) {
	for _, tt := range []struct {
		ref    configfile.ModelRef
		served string
		want   string
	}{
		{"openrouter/openai/gpt-6.1-sol", "anthropic/claude-opus-5.5", "openrouter/anthropic/claude-opus-5.5"},
		{"openrouter/openai/gpt-6.1-sol", "openai/gpt-6.1-sol", "openrouter/openai/gpt-6.1-sol"},
		{"anthropic/claude-sonnet-5", "claude-sonnet-5", "anthropic/claude-sonnet-5"},
		{"openrouter/openai/gpt-6.1-sol", "", "openrouter/openai/gpt-6.1-sol"},
	} {
		if got := servedRef(tt.ref, tt.served); got != tt.want {
			t.Errorf("servedRef(%s, %q) = %s, want %s", tt.ref, tt.served, got, tt.want)
		}
	}
}
