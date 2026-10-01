package adapter

import (
	"errors"
	"slices"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/model"
)

func TestSteppersBuildOnce(t *testing.T) {
	builds := 0
	c := &Steppers{Build: func(configfile.Provider) (model.Stepper, error) {
		builds++
		return &model.OpenAI{}, nil
	}}
	f := &configfile.File{Providers: map[string]configfile.Provider{"p": {Type: configfile.ProviderAnthropic}}}
	for range 3 {
		if _, err := c.Stepper(f, nil, "p"); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1", builds)
	}
	if _, err := c.Stepper(&configfile.File{}, nil, "p"); err == nil {
		t.Fatal("an undeclared provider must be an error")
	}
}

// TestSteppersAccountProviders: an account's own provider is its, even when
// another account's has the same name, and the file's providers stay
// shared.
func TestSteppersAccountProviders(t *testing.T) {
	var built []configfile.ProviderType
	c := &Steppers{Build: func(p configfile.Provider) (model.Stepper, error) {
		built = append(built, p.Type)
		return &model.OpenAI{}, nil
	}}
	f := &configfile.File{Providers: map[string]configfile.Provider{"shared": {Type: configfile.ProviderOpenRouter}}}
	alpha := &configfile.Account{Forge: configfile.ForgeGitHub, Name: "alpha", Providers: map[string]configfile.Provider{"own": {Type: configfile.ProviderOpenAI}}}
	beta := &configfile.Account{Forge: configfile.ForgeGitHub, Name: "beta", Providers: map[string]configfile.Provider{"own": {Type: configfile.ProviderAnthropic}}}
	for _, call := range []struct {
		t    *configfile.Account
		name string
	}{{alpha, "own"}, {beta, "own"}, {alpha, "shared"}, {beta, "shared"}, {alpha, "own"}} {
		if _, err := c.Stepper(f, call.t, call.name); err != nil {
			t.Fatal(err)
		}
	}
	want := []configfile.ProviderType{configfile.ProviderOpenAI, configfile.ProviderAnthropic, configfile.ProviderOpenRouter}
	if !slices.Equal(built, want) {
		t.Fatalf("built %v, want %v: one per account's own provider and one shared", built, want)
	}
	if _, err := c.Stepper(f, beta, "missing"); err == nil {
		t.Fatal("an undeclared provider must be an error")
	}
}

func TestBuildStepper(t *testing.T) {
	for _, typ := range []configfile.ProviderType{configfile.ProviderOpenRouter, configfile.ProviderOpenAI, configfile.ProviderAnthropic} {
		t.Run(string(typ), func(t *testing.T) {
			if _, err := BuildStepper(configfile.Provider{Type: typ}); err == nil {
				t.Fatal("a provider without a key must not build")
			}
		})
	}
}

func TestEmbeddersBuildOnce(t *testing.T) {
	builds := 0
	e := &Embedders{Build: func(configfile.Embedding) model.Embedder {
		builds++
		return &model.OpenAIEmbedder{}
	}}
	if got, spec := e.Embedder(&configfile.File{}); got != nil || spec != nil || builds != 0 {
		t.Fatalf("no embedder configured: %v, %v, %d builds", got, spec, builds)
	}
	f := &configfile.File{Embedding: &configfile.Embedding{BaseURL: "https://embed.example/v1", Model: "m", Dims: 8}}
	for range 3 {
		got, spec := e.Embedder(f)
		if got == nil || spec == nil || spec.Model != "m" {
			t.Fatalf("Embedder = %v, %+v", got, spec)
		}
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1", builds)
	}
	var none *Embedders
	if got, _ := none.Embedder(f); got != nil {
		t.Fatal("a nil Embedders resolves no embedder")
	}
}

func TestOutcomeAndServedRef(t *testing.T) {
	if Outcome(nil) != "ok" || Outcome(errors.New("x")) != "error" {
		t.Fatal("Outcome")
	}
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
		if got := ServedRef(tt.ref, tt.served); got != tt.want {
			t.Errorf("ServedRef(%s, %q) = %s, want %s", tt.ref, tt.served, got, tt.want)
		}
	}
}

// minimalFile is the rest of a configuration file a provider sits in.
const minimalFile = `apps:
  - name: acme-bot
    accounts: [acme]
    clientId: Iv1.test
    privateKey: { env: TEST_PROVIDER_KEY }
    webhookSecret: { env: TEST_PROVIDER_KEY }
`

func TestMask(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "sk-provider")
	t.Setenv("TEST_EGRESS_TOKEN", "ghp-egress")
	f, err := configfiletest.Parse(t, `providers:
  p:
    type: openai
    baseUrl: https://kritik:url-secret@llm.example/v1
    apiKey: { env: TEST_PROVIDER_KEY }
egress:
  allowHosts: [api.example.com]
  credentials:
    api.example.com: { env: TEST_EGRESS_TOKEN }
`+minimalFile)
	if err != nil {
		t.Fatal(err)
	}
	mask := Mask(f, f.Providers["p"], "krk_run", "", `se"cr\et<x`)
	tests := map[string]string{
		"key sk-provider":                           "key ***",
		"https://kritik:url-secret@llm/":            "https://***@llm/",
		"auth Bearer ghp-egress or bare ghp-egress": "auth *** or bare ***",
		"token krk_run":                             "token ***",
		`plain se"cr\et<x`:                          "plain ***",
		`{"k":"se\"cr\\et\u003cx"}`:                 `{"k":"***"}`,
		`{"k":"se\"cr\\et<x"}`:                      `{"k":"***"}`,
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
