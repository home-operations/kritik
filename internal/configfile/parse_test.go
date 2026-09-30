package configfile

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/model"
)

// fileConnection is a connection named name serving accounts, its secrets
// read from TEST_PRIVATE_KEY and TEST_WEBHOOK_SECRET.
func fileConnection(name string, accounts ...string) string {
	return "  - { name: " + name + ", forge: github, accounts: [" + strings.Join(accounts, ", ") + "], app: { clientId: Iv1." + name +
		", privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } } }\n"
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
// its connections serve, keeping the entries none serves aside.
func TestParse(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_KEY", "sk")
	f, err := Parse([]byte(`providers:
  p: { type: openai, apiKey: { env: TEST_KEY } }
defaults: { models: { review: p/big }, settle: 2m }
connections:
` + fileConnection("acme-bot", "acme") + fileConnection("org-bot", "org-2", "Org-3") + `accounts:
  - { forge: github, name: ORG-2, limits: { reviewsPerDay: 5 }, repositories: [{ name: repo-1, mode: agentic }] }
  - { forge: github, name: gone, forks: true }
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
	if !ok || org2.Limits.ReviewsPerDay == nil || f.Settings(org2, "org-2/repo-1").Mode != ReviewAgentic || f.Settings(org2, "").Settle != 2*time.Minute {
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
	conns := func(c ...string) string { return "connections:\n" + strings.Join(c, "") }
	for _, tt := range []struct{ name, yaml, want string }{
		{"an unknown key", "tenants: []\n", "field tenants not found"},
		{"a sealed reference", conns(strings.Replace(fileConnection("a", "x"), "{ env: TEST_PRIVATE_KEY }", "{ sealed: abc }", 1)),
			"field sealed not found"},
		{"env and file", conns(strings.Replace(fileConnection("a", "x"), "{ env: TEST_PRIVATE_KEY }", "{ env: TEST_PRIVATE_KEY, file: /x }", 1)),
			"set either env or file, not both"},
		{"a broken account entry", "accounts: [{ forge: github, name: acme, models: { review: nope/x } }]\n", `accounts[0].models.review references provider "nope"`},
		{"a repository entry that turns it on or off", "accounts: [{ forge: github, name: acme, repositories: [{ name: x, enabled: false }] }]\n",
			"accounts[0].repositories[0].enabled: turn a repository on or off in the dashboard"},
		{"a duplicate connection", conns(fileConnection("a", "x"), fileConnection("a", "y")), "names are hook paths"},
		{"an account two connections serve", conns(fileConnection("a", "x"), fileConnection("b", "X")), "an account is served by one connection"},
		{"two documents", "polling: {}\n---\npolling: {}\n", "one document"},
		{"an embedder with no endpoint", embeddingDoc("baseUrl: embed.example"), "embedding.baseUrl"},
		{"an embedder with no model", embeddingDoc(`model: ""`), "embedding.model is required"},
		{"an embedder too wide for the index", embeddingDoc("dims: 4096"), "embedding.dims must be between 1 and 4000"},
		{"an embedder with a negative bound", embeddingDoc("maxBatch: -1"), "must not be negative"},
		{"an embedder key that is not set", embeddingDoc("apiKey: { env: TEST_UNSET_KEY }"), "embedding.apiKey"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// embeddingDoc is a file whose embedder is valid but for override, a field
// that replaces the one of its name.
func embeddingDoc(override string) string {
	fields := map[string]string{
		"baseUrl": "baseUrl: https://embed.example/v1", "apiKey": "apiKey: { env: TEST_KEY }", "model": "model: voyage-code-3", "dims": "dims: 1024",
	}
	key, _, _ := strings.Cut(override, ":")
	fields[key] = override
	return "embedding:\n  " + strings.Join(slices.Sorted(maps.Values(fields)), "\n  ") + "\n"
}

func TestParseEmbedding(t *testing.T) {
	t.Setenv("TEST_KEY", "ek")
	f, err := Parse([]byte(embeddingDoc("maxBatch: 8")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := f.Embedding
	if e == nil || e.Model != "voyage-code-3" || e.Dims != 1024 || e.APIKeyValue().Value() != "ek" {
		t.Fatalf("embedding = %+v", e)
	}
	if batch, chars, item := e.Bounds(); batch != 8 || chars != model.DefaultEmbedMaxBatchChars || item != model.DefaultEmbedMaxItemChars {
		t.Fatalf("Bounds = %d, %d, %d", batch, chars, item)
	}
}

func TestParseAccountProviders(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_KEY", "sk-acme")
	f, err := Parse([]byte("connections:\n" + fileConnection("acme-bot", "acme") + `accounts:
  - forge: github
    name: acme
    providers: { own: { type: anthropic, apiKey: { env: TEST_KEY } } }
    models: { review: own/big }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p, ok := f.Provider(&f.Accounts[0], "own"); !ok || p.APIKeyValue().Value() != "sk-acme" {
		t.Fatalf("Provider(acme, own) = %+v, %v", p, ok)
	}
}

// TestConnectionEnv: the KRITIK_CONNECTIONS_* variables declare one
// connection, replacing the file's of its name or joining them, and a
// variable naming no key is refused.
func TestConnectionEnv(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	key := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(key, []byte("pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(t *testing.T, name string) {
		t.Helper()
		if name != "" {
			t.Setenv("KRITIK_CONNECTIONS_NAME", name)
		}
		t.Setenv("KRITIK_CONNECTIONS_ACCOUNTS", "org-1, user-1,")
		t.Setenv("KRITIK_CONNECTIONS_APP_CLIENT_ID", "Iv1.env")
		t.Setenv("KRITIK_CONNECTIONS_APP_PRIVATE_KEY_FILE", key)
		t.Setenv("KRITIK_CONNECTIONS_APP_WEBHOOK_SECRET", "from-env")
	}
	file := []byte("connections:\n" + fileConnection("acme-bot", "acme"))

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
	t.Run("a secret set twice", func(t *testing.T) {
		env(t, "")
		t.Setenv("KRITIK_CONNECTIONS_APP_PRIVATE_KEY", "pem")
		if _, err := Parse(file); err == nil || !strings.Contains(err.Error(), "set the same connection setting") {
			t.Fatalf("Parse = %v", err)
		}
	})
	t.Run("a variable naming nothing", func(t *testing.T) {
		t.Setenv("KRITIK_CONNECTIONS_APP_KEY", "x")
		if _, err := Parse(file); err == nil || !strings.Contains(err.Error(), "KRITIK_CONNECTIONS_APP_KEY names no connection setting") {
			t.Fatalf("Parse = %v", err)
		}
	})
}
