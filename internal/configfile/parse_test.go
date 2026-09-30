package configfile

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// fileApp is an app named name serving accounts, its secrets read from
// TEST_PRIVATE_KEY and TEST_WEBHOOK_SECRET.
func fileApp(name string, accounts ...string) string {
	return "  - { name: " + name + ", accounts: [" + strings.Join(accounts, ", ") + "], clientId: Iv1." + name +
		", privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }\n"
}

func TestParseEmpty(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("  \n")} {
		f, err := Parse(raw)
		if err != nil || len(f.Connections) != 0 || f.Hash() == "" {
			t.Fatalf("Parse(%q) = %+v, %v", raw, f, err)
		}
	}
}

// TestParse: the file holds the whole configuration, and runs the accounts
// its apps serve, keeping the entries none serves aside.
func TestParse(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_KEY", "sk")
	f, err := Parse([]byte(`providers:
  p: { type: openai, apiKey: { env: TEST_KEY } }
defaults: { models: { review: p/big }, settle: 2m }
apps:
` + fileApp("acme-bot", "acme") + fileApp("org-bot", "org-2", "Org-3") + `repositories:
  ORG-2/repo-1: { forks: true }
  gone/*: { forks: true }
accounts:
  ORG-2: { limits: { reviewsPerDay: 5 } }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Providers["p"].APIKeyValue().Value() != "sk" {
		t.Fatalf("providers = %+v", f.Providers)
	}
	accounts := make([]string, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		accounts = append(accounts, a.Slug())
	}
	if !slices.Equal(accounts, []string{"github/acme", "github/ORG-2", "github/Org-3"}) {
		t.Fatalf("accounts = %v", accounts)
	}
	org2, ok := f.Account(ForgeGitHub, "org-2")
	if !ok || org2.Limits.ReviewsPerDay == nil || !f.Settings(org2, "org-2/repo-1").Forks || f.Settings(org2, "").Settle != 2*time.Minute {
		t.Fatalf("org-2 = %+v", org2)
	}
	if in := f.ConnectionFor(org2); in == nil || in.Name != "org-bot" {
		t.Fatalf("ConnectionFor(org-2) = %v", in)
	}
	if u := f.Unserved(); len(u) != 1 || u[0].Name != "gone" {
		t.Fatalf("unserved = %+v", u)
	}
	if _, ok := f.Account(ForgeGitHub, "gone"); ok {
		t.Fatal("an unserved account runs")
	}
}

func TestParseRejectsEntries(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_KEY", "ek")
	apps := func(c ...string) string { return "apps:\n" + strings.Join(c, "") }
	for _, tt := range []struct{ name, yaml, want string }{
		{"an unknown key", "tenants: []\n", "field tenants not found"},
		{"connections is now apps", "connections: []\n", "field connections not found"},
		{"a sealed reference", apps(strings.Replace(fileApp("a", "x"), "{ env: TEST_PRIVATE_KEY }", "{ sealed: abc }", 1)),
			"field sealed not found"},
		{"a clientId from a file", apps(strings.Replace(fileApp("a", "x"), "clientId: Iv1.a", "clientId: { file: /x }", 1)),
			"field file not found"},
		{"a broken owner/* entry", "repositories: { acme/*: { models: { review: nope/x } } }\n", `repositories.acme/*.models.review references provider "nope"`},
		{"a repository entry that turns it on or off", "repositories: { acme/x: { enabled: false } }\n",
			"repositories.acme/x.enabled: turn a repository on or off in the dashboard"},
		{"a duplicate app", apps(fileApp("a", "x"), fileApp("a", "y")), "names are hook paths"},
		{"an account two apps serve", apps(fileApp("a", "x"), fileApp("b", "X")), "an account is served by one app"},
		{"two documents", "egress: {}\n---\negress: {}\n", "one document"},
		{"an embedder without a provider", embeddingDoc("model: voyage-code-3"), `embedding.model must be "<provider>/<model>"`},
		{"an embedder of an undeclared provider", embeddingDoc("model: other/voyage-code-3"), `references provider "other"`},
		{"an embedder of an anthropic provider", "providers:\n  a: { type: anthropic, apiKey: { env: TEST_KEY } }\nembedding: { model: a/x, dims: 8 }\n",
			"embeddings need an openrouter or openai one"},
		{"an embedder too wide for the index", embeddingDoc("dims: 4096"), "embedding.dims must be between 1 and 4000"},
		{"an embedder with a negative bound", embeddingDoc("maxBatch: -1"), "must not be negative"},
		{"an embedder with an endpoint of its own", embeddingDoc("baseUrl: https://embed.example/v1"), "field baseUrl not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// embeddingDoc is a file with an openrouter provider and an embedder of
// it that is valid but for override, a field that replaces the one of its
// name.
func embeddingDoc(override string) string {
	fields := map[string]string{"model": "model: or/voyage-code-3", "dims": "dims: 1024"}
	key, _, _ := strings.Cut(override, ":")
	fields[key] = override
	return "providers:\n  or: { type: openrouter, apiKey: { env: TEST_KEY } }\n" +
		"embedding:\n  " + strings.Join(slices.Sorted(maps.Values(fields)), "\n  ") + "\n"
}

// TestParseEmbedding: the embedder takes its provider's endpoint, or the
// type's, and its key, and the model's id on it.
func TestParseEmbedding(t *testing.T) {
	t.Setenv("TEST_KEY", "ek")
	f, err := Parse([]byte(embeddingDoc("maxBatch: 8")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := f.Embedding
	if e == nil || e.Model != "voyage-code-3" || e.BaseURL != model.OpenRouterBaseURL || e.Dims != 1024 || e.APIKeyValue().Value() != "ek" {
		t.Fatalf("embedding = %+v", e)
	}
	if batch, chars, item := e.Bounds(); batch != 8 || chars != model.DefaultEmbedMaxBatchChars || item != model.DefaultEmbedMaxItemChars {
		t.Fatalf("Bounds = %d, %d, %d", batch, chars, item)
	}
	f, err = Parse([]byte("providers:\n  gw: { type: openai, baseUrl: https://gw.example/v1, apiKey: { env: TEST_KEY } }\n" +
		"embedding: { model: gw/embed-large, dims: 8 }\n"))
	if err != nil || f.Embedding.BaseURL != "https://gw.example/v1" || f.Embedding.Model != "embed-large" {
		t.Fatalf("embedding = %+v, %v", f.Embedding, err)
	}
}

func TestParseAccountProviders(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_KEY", "sk-acme")
	f, err := Parse([]byte("apps:\n" + fileApp("acme-bot", "acme") + `accounts:
  acme:
    providers: { own: { type: anthropic, apiKey: { env: TEST_KEY } } }
repositories:
  acme/*: { models: { review: own/big } }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p, ok := f.Provider(&f.Accounts[0], "own"); !ok || p.APIKeyValue().Value() != "sk-acme" {
		t.Fatalf("Provider(acme, own) = %+v, %v", p, ok)
	}
}

// TestConnectionEnv: the KRITIK_APPS_* variables declare one app,
// replacing the file's of its name or joining them, and a variable naming
// no key is refused.
func TestConnectionEnv(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	env := func(t *testing.T, name string) {
		t.Helper()
		if name != "" {
			t.Setenv("KRITIK_APPS_NAME", name)
		}
		t.Setenv("KRITIK_APPS_ACCOUNTS", "org-1, user-1,")
		t.Setenv("KRITIK_APPS_CLIENT_ID", "Iv1.env")
		t.Setenv("KRITIK_APPS_PRIVATE_KEY", "pem\n")
		t.Setenv("KRITIK_APPS_WEBHOOK_SECRET", "from-env")
	}
	file := []byte("apps:\n" + fileApp("acme-bot", "acme"))

	t.Run("joins the file's", func(t *testing.T) {
		env(t, "")
		f, err := Parse(file)
		if err != nil {
			t.Fatal(err)
		}
		in, ok := f.Connection(DefaultEnvConnection)
		if !ok || len(f.Connections) != 2 || !slices.Equal(in.Accounts, []string{"org-1", "user-1"}) ||
			in.App.ClientIDValue() != "Iv1.env" || in.App.PrivateKeyValue().Value() != "pem" || in.WebhookSecretValue().Value() != "from-env" {
			t.Fatalf("connections = %+v", f.Connections)
		}
		if !f.ConnectionFromEnv(DefaultEnvConnection) || f.ConnectionFromEnv("acme-bot") {
			t.Fatal("ConnectionFromEnv")
		}
	})
	t.Run("replaces the file's of its name", func(t *testing.T) {
		env(t, "acme-bot")
		f, err := Parse(file)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Connections) != 1 || f.Connections[0].Serves("acme") || !f.Connections[0].Serves("org-1") {
			t.Fatalf("connections = %+v", f.Connections)
		}
	})
	t.Run("a secret from a file", func(t *testing.T) {
		env(t, "")
		t.Setenv("KRITIK_APPS_PRIVATE_KEY_FILE", "/var/run/secrets/key.pem")
		if _, err := Parse(file); err == nil || !strings.Contains(err.Error(), "KRITIK_APPS_PRIVATE_KEY_FILE names no app setting") {
			t.Fatalf("Parse = %v", err)
		}
	})
	t.Run("a variable naming nothing", func(t *testing.T) {
		t.Setenv("KRITIK_APPS_KEY", "x")
		if _, err := Parse(file); err == nil || !strings.Contains(err.Error(), "KRITIK_APPS_KEY names no app setting") {
			t.Fatalf("Parse = %v", err)
		}
	})
}
