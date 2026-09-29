package configfile

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// fileWithDefaults is minimal's connection with instance defaults in the
// file: a provider, both default models and an embedder.
const fileWithDefaults = `connections:
  - { name: acme-bot, forge: github, accounts: [acme], app: { clientId: x, privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } } }
providers:
  openrouter: { type: openrouter, apiKey: { env: TEST_PROVIDER_KEY } }
defaults:
  models: { review: openrouter/big, fallback: openrouter/small }
embedding: { baseUrl: https://embed.example/v1, apiKey: { env: TEST_PROVIDER_KEY }, model: e1, dims: 8 }
`

func setInstanceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_PROVIDER_KEY", "sk-file")
}

// TestFileInstanceDefaults: the file's providers, default models and
// embedder run when the spec sets none, every account inherits them from
// the file, and the spec overrides a provider by name, a model by key and
// the embedder whole.
func TestFileInstanceDefaults(t *testing.T) {
	setInstanceEnv(t)
	base, err := Parse([]byte(fileWithDefaults))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f, err := Merge(base, spec(`{"accounts":[{"forge":"github","name":"acme"}]}`, 1), testOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	a := &f.Accounts[0]
	if p, ok := f.Provider(a, "openrouter"); !ok || p.APIKeyValue().Value() != "sk-file" {
		t.Fatalf("provider = %+v, %v; want the file's", p, ok)
	}
	s := f.Settings(a, "acme/x")
	if s.Models.Review != "openrouter/big" || s.Models.Fallback != "openrouter/small" {
		t.Fatalf("models = %+v; want the file's", s.Models)
	}
	if src := f.Sources(a, "acme/x"); src["models.review"] != SourceFile || src["models.fallback"] != SourceFile {
		t.Fatalf("sources = %v; want the file's models from the file", src)
	}
	if f.Embedding == nil || f.Embedding.Model != "e1" || f.Embedding.APIKeyValue().Value() != "sk-file" {
		t.Fatalf("embedding = %+v; want the file's", f.Embedding)
	}

	over := `{"providers":{"openrouter":{"type":"openrouter","baseUrl":"https://router.example/api/v1","apiKey":{"sealed":"test:sk-ui"}}},` +
		`"defaults":{"models":{"review":"openrouter/huge"}},` +
		`"embedding":{"baseUrl":"https://other.example/v1","apiKey":{"sealed":"test:sk-emb"},"model":"e2","dims":16},` +
		`"accounts":[{"forge":"github","name":"acme"}]}`
	if f, err = Merge(base, spec(over, 2), testOpener{}); err != nil {
		t.Fatalf("Merge over: %v", err)
	}
	a = &f.Accounts[0]
	if p, _ := f.Provider(a, "openrouter"); p.APIKeyValue().Value() != "sk-ui" || p.BaseURL != "https://router.example/api/v1" {
		t.Fatalf("provider = %+v; want the spec's in place of the file's", p)
	}
	s = f.Settings(a, "acme/x")
	if s.Models.Review != "openrouter/huge" || s.Models.Fallback != "openrouter/small" {
		t.Fatalf("models = %+v; want the spec's review model and the file's fallback", s.Models)
	}
	if src := f.Sources(a, "acme/x"); src["models.review"] != SourceDefaults || src["models.fallback"] != SourceFile {
		t.Fatalf("sources = %v", src)
	}
	if f.Embedding.Model != "e2" || f.Embedding.Dims != 16 {
		t.Fatalf("embedding = %+v; want the spec's whole", f.Embedding)
	}
	layer := f.FileLayer()
	if p := layer.Providers["openrouter"]; p.Source != SourceFile || !p.Overridden ||
		layer.Review != (FileValue{Value: "openrouter/big", Source: SourceFile, Overridden: true}) ||
		layer.Fallback.Overridden || layer.Embedding == nil || layer.Embedding.Model != "e1" || !layer.Embedding.Overridden {
		t.Fatalf("file layer = %+v; want the file's own values, marked where the spec overrides them", layer)
	}

	// An account's own provider may not take a name the file's providers
	// use, as with the spec's.
	mine := `{"accounts":[{"forge":"github","name":"acme","providers":{"openrouter":{"type":"openai","apiKey":{"sealed":"test:k"}}}}]}`
	if _, err := Merge(base, spec(mine, 3), testOpener{}); err == nil || !strings.Contains(err.Error(), "the instance declares a provider by that name") {
		t.Fatalf("Merge with an account provider named like the file's = %v", err)
	}
}

// TestInstanceDefaultsEnv: the environment declares one provider, the
// default models and the embedder, reported as the environment's, over
// the file's, and refuses a variable naming no key.
func TestInstanceDefaultsEnv(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv("KRITIK_PROVIDERS_API_KEY", "sk-env")
	t.Setenv("KRITIK_DEFAULTS_MODELS_REVIEW", "openrouter/env-model")
	t.Setenv("KRITIK_EMBEDDING_MODEL", "e-env")
	t.Setenv("KRITIK_EMBEDDING_DIMS", "32")
	base, err := Parse([]byte(fileWithDefaults))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f, err := Merge(base, spec(`{"accounts":[{"forge":"github","name":"acme"}]}`, 1), testOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	a := &f.Accounts[0]
	if p, _ := f.Provider(a, "openrouter"); p.APIKeyValue().Value() != "sk-env" || p.Type != ProviderOpenRouter {
		t.Fatalf("provider = %+v; want the environment's, typed after its default name", p)
	}
	if s := f.Settings(a, "acme/x"); s.Models.Review != "openrouter/env-model" || s.Models.Fallback != "openrouter/small" {
		t.Fatalf("models = %+v; want the environment's review model over the file's fallback", s.Models)
	}
	if src := f.Sources(a, "acme/x"); src["models.review"] != SourceEnv || src["models.fallback"] != SourceFile {
		t.Fatalf("sources = %v", src)
	}
	// The environment sets the embedder key by key over the file's.
	if e := f.Embedding; e.Model != "e-env" || e.Dims != 32 || e.BaseURL != "https://embed.example/v1" {
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
// neither the file nor the spec declares is refused once merged.
func TestFileDefaultModelNeedsAProvider(t *testing.T) {
	setInstanceEnv(t)
	base, err := Parse([]byte(strings.Replace(fileWithDefaults, "review: openrouter/big", "review: nowhere/big", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Merge(base, spec(`{}`, 1), testOpener{}); err == nil || !strings.Contains(err.Error(), "defaults.models.review") {
		t.Fatalf("Merge = %v; want the default model refused", err)
	}
}

// TestFileReviewDefaults: the file and the environment set the defaults'
// mode, thoroughness, forks and settle, which accounts inherit with the
// file's or the environment's source and the spec overrides key by key.
func TestFileReviewDefaults(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv("KRITIK_DEFAULTS_SETTLE", "45s")
	models := "  models: { review: openrouter/big, fallback: openrouter/small }\n"
	withDefaults := func(extra string) []byte { return []byte(strings.Replace(fileWithDefaults, models, models+extra, 1)) }
	base, err := Parse(withDefaults("  mode: agentic\n  forks: true\n  review: { thoroughness: focused }\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f, err := Merge(base, spec(`{"defaults":{"review":{"thoroughness":"thorough"}},"accounts":[{"forge":"github","name":"acme"}]}`, 1), testOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	a := &f.Accounts[0]
	s := f.Settings(a, "acme/x")
	if s.Mode != ReviewAgentic || !s.Forks || s.Settle != 45*time.Second || s.Review.Thoroughness != ThoroughnessThorough {
		t.Fatalf("settings = mode %s forks %v settle %s thoroughness %s; want the file's and the environment's, the spec's thoroughness",
			s.Mode, s.Forks, s.Settle, s.Review.Thoroughness)
	}
	src := f.Sources(a, "acme/x")
	for key, want := range map[string]Source{"mode": SourceFile, "forks": SourceFile, "settle": SourceEnv, "review.thoroughness": SourceDefaults} {
		if src[key] != want {
			t.Errorf("source of %s = %s, want %s", key, src[key], want)
		}
	}
	want := []FileDefault{
		{"mode", FileValue{Value: "agentic", Source: SourceFile}},
		{"review.thoroughness", FileValue{Value: "focused", Source: SourceFile, Overridden: true}},
		{"forks", FileValue{Value: "true", Source: SourceFile}},
		{"settle", FileValue{Value: "45s", Source: SourceEnv}},
	}
	if got := f.FileLayer().Defaults; !slices.Equal(got, want) {
		t.Fatalf("file layer defaults = %+v, want %+v", got, want)
	}
	if _, err := Merge(base, spec(`{}`, 2), testOpener{}); err != nil {
		t.Fatalf("Merge without a spec: %v", err)
	}
	bad, err := Parse(withDefaults("  mode: thorough\n"))
	if err == nil {
		_, err = Merge(bad, spec(`{}`, 1), testOpener{})
	}
	if err == nil || !strings.Contains(err.Error(), "defaults.mode must be single or agentic") {
		t.Fatalf("an unknown mode = %v", err)
	}
}
