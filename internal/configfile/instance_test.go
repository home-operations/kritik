package configfile

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// fileWithDefaults is minimal's app with instance defaults: a provider,
// both default models and an embedder of the provider, and acme's entry.
const fileWithDefaults = `apps:
  - { name: acme-bot, accounts: [acme], clientId: x, privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }
providers:
  openrouter: { type: openrouter, apiKey: { env: TEST_PROVIDER_KEY } }
defaults:
  models: { review: openrouter/big, fallback: openrouter/small }
embedding: { model: openrouter/e1, dims: 8 }
accounts:
  acme: {}
`

func setInstanceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_PROVIDER_KEY", "sk-file")
}

// TestFileInstanceDefaults: the file's providers, default models and
// embedder run, and every account inherits them.
func TestFileInstanceDefaults(t *testing.T) {
	setInstanceEnv(t)
	f, err := Parse([]byte(fileWithDefaults))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := &f.Accounts[0]
	if p, ok := f.Provider(a, "openrouter"); !ok || p.APIKeyValue().Value() != "sk-file" {
		t.Fatalf("provider = %+v, %v; want the file's", p, ok)
	}
	s := f.Settings(a, "acme/x")
	if s.Models.Review != "openrouter/big" || s.Models.Fallback != "openrouter/small" {
		t.Fatalf("models = %+v; want the file's", s.Models)
	}
	if src := f.Sources(a, "acme/x"); src["models.review"] != SourceDefaults || src["models.fallback"] != SourceDefaults {
		t.Fatalf("sources = %v; want the models from the defaults", src)
	}
	if f.Embedding == nil || f.Embedding.Model != "e1" || f.Embedding.APIKeyValue().Value() != "sk-file" || f.Embedding.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("embedding = %+v; want the file's", f.Embedding)
	}
	layer := f.FileLayer()
	if p := layer.Providers["openrouter"]; p.Source != SourceFile || layer.Review != (FileValue{Value: "openrouter/big", Source: SourceFile}) ||
		layer.Embedding == nil || layer.Embedding.Model != "openrouter/e1" {
		t.Fatalf("file layer = %+v", layer)
	}

	// An account's own provider may not take a name the instance's use.
	mine := strings.Replace(fileWithDefaults, "  acme: {}",
		"  acme: { providers: { openrouter: { type: openai, apiKey: { env: TEST_PROVIDER_KEY } } } }", 1)
	if _, err := Parse([]byte(mine)); err == nil || !strings.Contains(err.Error(), "the instance declares a provider by that name") {
		t.Fatalf("Parse with an account provider named like the instance's = %v", err)
	}
}

// TestInstanceDefaultsEnv: the environment declares one provider, the
// default models and the embedder, reported as the environment's, over
// the file's, and refuses a variable naming no key.
func TestInstanceDefaultsEnv(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv("KRITIK_PROVIDERS_API_KEY", "sk-env")
	t.Setenv("KRITIK_DEFAULTS_MODELS_REVIEW", "openrouter/env-model")
	t.Setenv("KRITIK_EMBEDDING_MODEL", "openrouter/e-env")
	t.Setenv("KRITIK_EMBEDDING_DIMS", "32")
	f, err := Parse([]byte(fileWithDefaults))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := &f.Accounts[0]
	if p, _ := f.Provider(a, "openrouter"); p.APIKeyValue().Value() != "sk-env" || p.Type != ProviderOpenRouter {
		t.Fatalf("provider = %+v; want the environment's, typed after its default name", p)
	}
	if s := f.Settings(a, "acme/x"); s.Models.Review != "openrouter/env-model" || s.Models.Fallback != "openrouter/small" {
		t.Fatalf("models = %+v; want the environment's review model over the file's fallback", s.Models)
	}
	if src := f.Sources(a, "acme/x"); src["models.review"] != SourceEnv || src["models.fallback"] != SourceDefaults {
		t.Fatalf("sources = %v", src)
	}
	// The environment sets the embedder key by key over the file's, on the
	// environment's key.
	if e := f.Embedding; e.Model != "e-env" || e.Dims != 32 || e.APIKeyValue().Value() != "sk-env" {
		t.Fatalf("embedding = %+v", e)
	}
	layer := f.FileLayer()
	if layer.Providers["openrouter"].Source != SourceEnv || layer.Review.Source != SourceEnv || layer.Embedding.Source != SourceEnv {
		t.Fatalf("file layer = %+v; want the environment's values as its", layer)
	}

	for _, tt := range []struct{ name, key, value, want string }{
		{"an unknown provider key", "KRITIK_PROVIDERS_MODEL", "x", "KRITIK_PROVIDERS_MODEL names no provider setting"},
		{"an unknown defaults key", "KRITIK_DEFAULTS_FILTER", "true", "KRITIK_DEFAULTS_FILTER names no defaults setting"},
		{"forks that are not a bool", "KRITIK_DEFAULTS_FORKS", "sometimes", "KRITIK_DEFAULTS_FORKS must be true or false"},
		{"a settle that is not a duration", "KRITIK_DEFAULTS_SETTLE", "soon", "KRITIK_DEFAULTS_SETTLE"},
		{"an unknown embedding key", "KRITIK_EMBEDDING_URL", "x", "KRITIK_EMBEDDING_URL names no embedding setting"},
		{"dims that are not a number", "KRITIK_EMBEDDING_DIMS", "many", "KRITIK_EMBEDDING_DIMS must be a whole number"},
		{"a key set twice", "KRITIK_PROVIDERS_API_KEY_FILE", "/nope", "set the same provider setting"},
		{"a name that is no type, without one", "KRITIK_PROVIDERS_NAME", "router", "providers.router.type must be"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := Parse([]byte(fileWithDefaults)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v; want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestFileDefaultModelNeedsAProvider: a default model naming a provider
// the file does not declare is refused.
func TestFileDefaultModelNeedsAProvider(t *testing.T) {
	setInstanceEnv(t)
	_, err := Parse([]byte(strings.Replace(fileWithDefaults, "review: openrouter/big", "review: nowhere/big", 1)))
	if err == nil || !strings.Contains(err.Error(), "defaults.models.review") {
		t.Fatalf("Parse = %v; want the default model refused", err)
	}
}

// TestFileReviewDefaults: the file and the environment set the defaults'
// mode, feedback, forks and settle, which accounts inherit with the
// defaults' or the environment's source.
func TestFileReviewDefaults(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv("KRITIK_DEFAULTS_SETTLE", "45s")
	models := "  models: { review: openrouter/big, fallback: openrouter/small }\n"
	withDefaults := func(extra string) []byte { return []byte(strings.Replace(fileWithDefaults, models, models+extra, 1)) }
	f, err := Parse(withDefaults("  mode: agentic\n  forks: true\n  feedback: minimal\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := &f.Accounts[0]
	s := f.Settings(a, "acme/x")
	if s.Mode != ReviewAgentic || !s.Forks || s.Settle != 45*time.Second || s.Review.Feedback != FeedbackMinimal {
		t.Fatalf("settings = mode %s forks %v settle %s feedback %s; want the file's and the environment's",
			s.Mode, s.Forks, s.Settle, s.Review.Feedback)
	}
	src := f.Sources(a, "acme/x")
	for key, want := range map[string]Source{"mode": SourceDefaults, "forks": SourceDefaults, "settle": SourceEnv, "feedback": SourceDefaults} {
		if src[key] != want {
			t.Errorf("source of %s = %s, want %s", key, src[key], want)
		}
	}
	want := []FileDefault{
		{"mode", FileValue{Value: "agentic", Source: SourceFile}},
		{"feedback", FileValue{Value: "minimal", Source: SourceFile}},
		{"forks", FileValue{Value: "true", Source: SourceFile}},
		{"settle", FileValue{Value: "45s", Source: SourceEnv}},
	}
	if got := f.FileLayer().Defaults; !slices.Equal(got, want) {
		t.Fatalf("file layer defaults = %+v, want %+v", got, want)
	}
	if _, err := Parse(withDefaults("  mode: thorough\n")); err == nil || !strings.Contains(err.Error(), "defaults.mode must be single or agentic") {
		t.Fatalf("an unknown mode = %v", err)
	}
}
