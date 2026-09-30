package configfile

import (
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/model"
)

// fixture materialises testdata/full.yaml with its file references pointing
// into a temp dir and its env references set, and returns its content.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local-key"), []byte("sk-ant-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private-key.pem"), []byte("-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_CLIENT_ID", "Iv1.fromenv")
	return strings.ReplaceAll(string(raw), "__DIR__", dir)
}

func TestLoadFull(t *testing.T) {
	f, err := load(t, fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Run("providers resolve from env and file", func(t *testing.T) {
		if got := f.Providers["openrouter"].APIKeyValue().Value(); got != "sk-or-test" {
			t.Fatalf("openrouter key = %q", got)
		}
		if got := f.Providers["local"].APIKeyValue().Value(); got != "sk-ant-test" {
			t.Fatalf("local key = %q (trailing newline must be trimmed)", got)
		}
	})

	t.Run("secrets redact when formatted", func(t *testing.T) {
		s := f.Providers["openrouter"].APIKeyValue()
		for _, out := range []string{s.String(), fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%+v", s)} {
			if strings.Contains(out, "sk-or") {
				t.Fatalf("secret leaked through formatting: %q", out)
			}
		}
		if (Secret{}).String() != "" {
			t.Fatal("an empty secret should format as empty, so absence stays visible")
		}
	})

	t.Run("settings layer defaults, account, repository", func(t *testing.T) {
		ho, _ := f.Account(ForgeGitHub, "home-operations")
		od, _ := f.Account(ForgeGitHub, "onedr0p")
		tests := []struct {
			name     string
			account  *Account
			repo     string
			enabled  bool
			review   ModelRef
			forks    bool
			conc     int
			perDay   int
			settle   time.Duration
			filterOK map[string]any // a PR the effective filter must accept
			filterNo map[string]any // a PR the effective filter must reject
		}{
			{
				name: "unlisted repo inherits account", account: ho, repo: "home-operations/other",
				enabled: true, review: "openrouter/openai/gpt-6-sol", forks: false, conc: 3, perDay: 200,
				filterOK: SamplePR(), filterNo: with(SamplePR(), "draft", true),
			},
			{
				name: "listed repo applies its own settle", account: ho, repo: "home-operations/flate",
				enabled: true, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200,
				settle:   30 * time.Second,
				filterOK: SamplePR(),
			},
			{
				name: "repo filter replaces account filter", account: ho, repo: "home-operations/kopiur",
				enabled: true, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200,
				filterOK: SamplePR(), filterNo: with(SamplePR(), "labels", []any{map[string]any{"name": "skip-review", "color": "0"}}),
			},
			{
				name: "listed repo inherits enabled", account: ho, repo: "home-operations/charts-mirror",
				enabled: true, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200,
			},
			{
				name: "account overrides review model and forks", account: od, repo: "onedr0p/home-ops",
				enabled: true, review: "local/claude-opus-5", forks: true, conc: 3,
				settle:   2 * time.Minute,
				filterOK: SamplePR(),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := f.Settings(tt.account, tt.repo)
				if s.Enabled != tt.enabled || s.Models.Review != tt.review || s.Forks != tt.forks ||
					s.Limits.Concurrency != tt.conc || s.Limits.ReviewsPerDay != tt.perDay ||
					s.Settle != tt.settle {
					t.Fatalf("Settings = %+v", s)
				}
				if s.Models.Fallback != "local/claude-sonnet-5" {
					t.Fatalf("fallback should inherit from defaults, got %q", s.Models.Fallback)
				}
				if tt.filterOK != nil {
					if ok, err := s.Filter.Eval(tt.filterOK); err != nil || !ok {
						t.Fatalf("filter should accept: ok=%v err=%v", ok, err)
					}
				}
				if tt.filterNo != nil {
					if ok, err := s.Filter.Eval(tt.filterNo); err != nil || ok {
						t.Fatalf("filter should reject: ok=%v err=%v", ok, err)
					}
				}
			})
		}
	})

	t.Run("concurrency falls back to the default when unset everywhere", func(t *testing.T) {
		g := &File{Accounts: []Account{{Forge: ForgeGitHub, Name: "x"}}}
		if got := g.Settings(&g.Accounts[0], "x/y").Limits.Concurrency; got != DefaultConcurrency {
			t.Fatalf("concurrency = %d, want %d", got, DefaultConcurrency)
		}
	})
}

func TestConnectionCredentials(t *testing.T) {
	f, err := load(t, fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in, ok := f.Connection("sticky-gecko")
	if !ok || !in.Serves("Home-Operations") || in.Origin() != OriginFile {
		t.Fatalf("Connection(sticky-gecko) = %v, %v", in, ok)
	}
	if in.App.PrivateKeyValue().Value() == "" || in.WebhookSecretValue().Value() != "whsec" || in.App.ClientIDValue() != "Iv1.xxxxxxxx" {
		t.Fatal("github app credentials not resolved")
	}
	br, _ := f.Connection("bot-ross")
	if br.App.ClientIDValue() != "Iv1.fromenv" {
		t.Fatalf("clientIdFrom not resolved: %q", br.App.ClientIDValue())
	}
	if _, ok := f.Connection("nope"); ok {
		t.Fatal("unknown connection should not resolve")
	}
}

func TestHashAndConnectionLookup(t *testing.T) {
	f, err := load(t, fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Hash()) != 64 {
		t.Fatalf("hash = %q", f.Hash())
	}
	ho, ok := f.Account(ForgeGitHub, "Home-Operations")
	if !ok || ho.Name != "home-operations" || ho.ID() != AccountID(ForgeGitHub, "home-operations") || ho.Slug() != "github/home-operations" {
		t.Fatalf("Account = %+v, %v", ho, ok)
	}
	if in := f.ConnectionFor(ho); in == nil || in.Name != "sticky-gecko" {
		t.Fatalf("ConnectionFor = %v", in)
	}
	if f.ConnectionFor(&Account{Forge: ForgeGitHub, Name: "someone-else"}) != nil {
		t.Fatal("an account no connection serves must not resolve")
	}
	if _, ok := f.Account(ForgeGitHub, "someone-else"); ok {
		t.Fatal("an account no connection serves must not run")
	}
}

func TestRetentionAndIgnore(t *testing.T) {
	f, err := load(t, fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.DisabledIndexGrace() != 720*time.Hour {
		t.Fatalf("grace = %s", f.DisabledIndexGrace())
	}
	if (&File{}).DisabledIndexGrace() != DefaultDisabledIndexGrace {
		t.Fatal("unset grace should fall back to the default")
	}
	ho, _ := f.Account(ForgeGitHub, "home-operations")
	got := f.Settings(ho, "home-operations/flate").Ignore
	if len(got) != len(DefaultIgnore)+1 || got[len(got)-1] != "**/testdata/**" {
		t.Fatalf("ignore = %v", got)
	}
	if n := len(f.Settings(ho, "home-operations/other").Ignore); n != len(DefaultIgnore) {
		t.Fatalf("unlisted repo ignore = %d globs, want defaults only", n)
	}
}

func TestFilterOnBody(t *testing.T) {
	prg, err := compileFilter(`pr.body.contains("[skip-review]")`)
	if err != nil {
		t.Fatalf("compileFilter: %v", err)
	}
	marked := with(SamplePR(), "body", "please review\n\n[skip-review]")
	if ok, err := prg.Eval(marked); err != nil || !ok {
		t.Fatalf("filter should accept a body containing the marker: ok=%v err=%v", ok, err)
	}
	if ok, err := prg.Eval(SamplePR()); err != nil || ok {
		t.Fatalf("filter should reject the sample body: ok=%v err=%v", ok, err)
	}
}

func with(pr map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(pr))
	maps.Copy(out, pr)
	out[k] = v
	return out
}

// minimal is the smallest valid file; cases mutate it.
var minimal = githubMinimal("clientId: Iv1.acme, ")

// githubMinimal is the smallest configuration: one connection, in the file,
// serving acme, and acme's entry in the spec last, so a case appends its
// keys; clientFields is spliced into the app block.
func githubMinimal(clientFields string) string {
	return `
connections:
  - name: acme-bot
    forge: github
    accounts: [acme]
    app: { ` + clientFields + `privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }
accounts:
  - forge: github
    name: acme
`
}

// acme is minimal with extra, lines of keys, added to acme's entry.
func acme(extra string) string { return minimal + extra }

// loadBytes is load for a document in bytes.
func loadBytes(t *testing.T, raw []byte) (*File, error) {
	t.Helper()
	return load(t, string(raw))
}

// TestEnabledDefault checks enabled resolves like the other overrides: the
// defaults and an account set where a repository starts, entry or not,
// until an admin turns it on or off.
func TestEnabledDefault(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	tests := []struct {
		name, doc string
		want      map[string]bool
	}{
		{"on unless turned off", acme("    repositories: [{ name: listed, mode: single }]\n"),
			map[string]bool{"acme/new": true, "acme/listed": true}},
		{"off at the defaults", "defaults: { enabled: false }\n" + acme("    repositories: [{ name: listed }]\n"),
			map[string]bool{"acme/new": false, "acme/listed": false}},
		{"the account over the defaults", "defaults: { enabled: false }\n" + acme("    enabled: true\n"),
			map[string]bool{"acme/new": true}},
		{"off at the account", acme("    enabled: false\n    repositories: [{ name: listed }]\n"),
			map[string]bool{"acme/new": false, "acme/listed": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := mustLoad(t, tt.doc)
			for repo, want := range tt.want {
				if got := f.Settings(&f.Accounts[0], repo).Enabled; got != want {
					t.Errorf("%s enabled = %v, want %v", repo, got, want)
				}
			}
		})
	}
}

// TestRuns checks which repositories run once the forge says what they
// are: an archived one never, one an admin turned on or off as they chose,
// a fork not otherwise, and the rest as enabled resolves.
func TestRuns(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	archived, fork := RepoTraits{Archived: true}, RepoTraits{Fork: true}
	tests := []struct {
		name, doc, repo string
		traits          RepoTraits
		want            bool
	}{
		{"a source repository", acme(""), "acme/app", RepoTraits{}, true},
		{"a source repository turned off", acme("    enabled: false\n"), "acme/app", RepoTraits{}, false},
		{"a fork", acme(""), "acme/copy", fork, false},
		{"a fork the account turns on", acme("    enabled: true\n"), "acme/copy", fork, false},
		{"a fork with an entry", acme("    repositories: [{ name: copy, mode: agentic }]\n"), "acme/copy", fork, false},
		{"an archived repository", acme(""), "acme/old", archived, false},
		{"a repository an admin turned off", acme("    enabled: true\n"), "acme/app", RepoTraits{TurnedOn: new(false)}, false},
		{"a repository an admin turned on", acme("    enabled: false\n"), "acme/app", RepoTraits{TurnedOn: new(true)}, true},
		{"a fork an admin turned on", acme(""), "acme/copy", RepoTraits{Fork: true, TurnedOn: new(true)}, true},
		{"an archived repository an admin turned on", acme(""), "acme/old", RepoTraits{Archived: true, TurnedOn: new(true)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := mustLoad(t, tt.doc)
			if got := f.Runs(&f.Accounts[0], tt.repo, tt.traits); got != tt.want {
				t.Errorf("Runs(%s, %+v) = %v, want %v", tt.repo, tt.traits, got, tt.want)
			}
		})
	}
}

func TestProviders(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	tests := []struct {
		name    string
		yaml    string
		want    Provider
		pricing model.Pricing
	}{
		{
			name: "anthropic with pricing",
			yaml: "  p:\n    type: anthropic\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n" +
				"    pricing:\n      acme-large: { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 }\n",
			want:    Provider{Type: ProviderAnthropic},
			pricing: model.Pricing{"acme-large": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
		},
		{
			name: "anthropic behind a gateway",
			yaml: "  p:\n    type: anthropic\n    baseUrl: https://gw.example.com/\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n",
			want: Provider{Type: ProviderAnthropic, BaseURL: "https://gw.example.com/"},
		},
		{
			name: "openai at the SDK default url",
			yaml: "  p:\n    type: openai\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n",
			want: Provider{Type: ProviderOpenAI},
		},
		{
			name: "openrouter behind a proxy",
			yaml: "  p:\n    type: openrouter\n    baseUrl: https://proxy.example.com/api/v1\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n",
			want: Provider{Type: ProviderOpenRouter, BaseURL: "https://proxy.example.com/api/v1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := loadBytes(t, []byte("providers:\n"+tt.yaml+minimal))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			p := f.Providers["p"]
			if p.Type != tt.want.Type || p.BaseURL != tt.want.BaseURL || p.APIKeyValue().Value() != "whsec" {
				t.Fatalf("provider = %+v", p)
			}
			if !maps.Equal(p.Pricing, tt.pricing) {
				t.Fatalf("pricing = %v, want %v", p.Pricing, tt.pricing)
			}
		})
	}
}

// TestAccountProviders: an account's own provider serves its models, and only
// its.
func TestAccountProviders(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	own := "    providers:\n      own: { type: openai, baseUrl: http://llm.internal:4000/v1, apiKey: { env: TEST_WEBHOOK_SECRET } }\n" +
		"    models: { review: own/big }\n"
	withOwn := func(extra string) string {
		return acme(own + extra)
	}
	f, err := loadBytes(t, []byte(withOwn("")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ten := &f.Accounts[0]
	if p, ok := f.Provider(ten, "own"); !ok || p.Type != ProviderOpenAI || p.APIKeyValue().Value() != "whsec" {
		t.Fatalf("Provider(acme, own) = %+v, %v", p, ok)
	}
	if _, ok := f.Provider(nil, "own"); ok {
		t.Fatal("an account's provider must not be the instance's")
	}
	refused := []struct{ name, yaml, want string }{
		{"a name a model reference cannot carry", strings.Replace(withOwn(""), "      own:", "      Own:", 1), "a provider name must be lowercase"},
		{"a name the instance already uses", "providers:\n  own: { type: anthropic, apiKey: { env: TEST_WEBHOOK_SECRET } }\n" + withOwn(""),
			"the instance declares a provider by that name"},
		{"the defaults naming an account's provider", "defaults:\n  models: { review: own/big }\n" + withOwn(""), "not declared under providers"},
		{"an invalid provider", strings.Replace(withOwn(""), "type: openai", "type: gemini", 1), "providers.own.type must be"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadBytes(t, []byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_EMPTY", "")

	if _, err := loadBytes(t, []byte(minimal)); err != nil {
		t.Fatalf("minimal fixture must parse: %v", err)
	}

	tests := []struct {
		name string
		yaml string
		want string // substring of the error
	}{
		{"unknown top-level key", minimal + "account: []\n", "field account not found"},
		{"unknown nested key", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme]\n    owner: acme", 1), "field owner not found"},
		{"bad connection name", strings.Replace(minimal, "name: acme-bot", "name: Acme Bot", 1), "lowercase"},
		{"duplicate connection", strings.Replace(minimal, "accounts:\n  - forge", "  - { name: acme-bot, forge: github, accounts: [other], app: { clientId: x, "+
			"privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } } }\naccounts:\n  - forge", 1), "names are hook paths"},
		{"an account two connections serve", strings.Replace(minimal, "accounts:\n  - forge", "  - { name: acme-two, forge: github, accounts: [ACME], app: { clientId: x, "+
			"privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } } }\naccounts:\n  - forge", 1), "an account is served by one connection"},
		{"duplicate account entry", acme("  - forge: github\n    name: Acme\n"), "duplicates accounts[0]"},
		{"an account of another forge", strings.Replace(minimal, "  - forge: github\n    name: acme", "  - forge: gitlab\n    name: acme", 1), "accounts[0].forge must be github"},
		{"an account name with its owner", strings.Replace(minimal, "    name: acme\n", "    name: acme/x\n", 1), "must be the account's name"},
		{"missing accounts", strings.Replace(minimal, "    accounts: [acme]\n", "", 1), "accounts must list at least one account"},
		{"blank account", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme, ' ']", 1), `accounts[1] " " must be the account's name`},
		{"a served account with its owner", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme/x]", 1), `accounts[0] "acme/x" must be the account's name`},
		{"account listed twice", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme, ACME]", 1), `accounts[1] "ACME" is listed twice`},
		{"unknown forge", strings.Replace(minimal, "forge: github", "forge: gitlab", 1), "forge must be github, got \"gitlab\""},
		{"github without app", strings.Replace(minimal, "    app: { clientId: Iv1.acme, privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }\n", "", 1), "needs an app"},
		{"github app with both client id forms", githubMinimal("clientId: x, clientIdFrom: { env: TEST_WEBHOOK_SECRET }, "), "exactly one of clientId or clientIdFrom"},
		{"github app with neither client id form", githubMinimal(""), "exactly one of clientId or clientIdFrom"},
		{"missing private key", strings.Replace(minimal, "privateKey: { env: TEST_PRIVATE_KEY }, ", "", 1), "app.privateKey: reference must set env or file"},
		{"unset env reference", strings.Replace(minimal, "TEST_PRIVATE_KEY", "TEST_DOES_NOT_EXIST", 1), "is not set"},
		{"empty env reference", strings.Replace(minimal, "TEST_PRIVATE_KEY", "TEST_EMPTY", 1), "privateKey is required"},
		{"missing file reference", strings.Replace(minimal, "{ env: TEST_PRIVATE_KEY }", "{ file: /nonexistent/token }", 1), "no such file"},
		{"env and file both set", strings.Replace(minimal, "{ env: TEST_PRIVATE_KEY }", "{ env: TEST_PRIVATE_KEY, file: /x }", 1), "not both"},
		{"empty reference", strings.Replace(minimal, "{ env: TEST_PRIVATE_KEY }", "{}", 1), "app.privateKey: reference must set env or file"},
		{"unknown provider type", "providers:\n  p:\n    type: cohere\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n" + minimal, "type must be"},
		{"negative pricing", "providers:\n  p:\n    type: anthropic\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n" +
			"    pricing: { acme-large: { input: 3, output: -1 } }\n" + minimal, "providers.p.pricing.acme-large"},
		{"unknown pricing field", "providers:\n  p:\n    type: anthropic\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n" +
			"    pricing: { acme-large: { prompt: 3 } }\n" + minimal, "field prompt not found"},
		{"relative base url", "providers:\n  p:\n    type: openai\n    baseUrl: gw.example.com/v1\n    apiKey: { env: TEST_WEBHOOK_SECRET }\n" + minimal,
			"must be an absolute URL"},
		{"anthropic without key", "providers:\n  p:\n    type: anthropic\n    apiKey: { env: TEST_EMPTY }\n" + minimal, "apiKey resolved to an empty value"},
		{"model without provider", "defaults:\n  models:\n    review: gpt\n" + minimal, "<provider>/<model>"},
		{"model referencing undeclared provider", "defaults:\n  models:\n    review: nope/gpt\n" + minimal, "not declared under providers"},
		{"account model referencing undeclared provider", acme("    models: { review: nope/gpt }\n"), "not declared under providers"},
		{"negative limit", "defaults:\n  limits:\n    reviewsPerDay: -1\n" + minimal, "must not be negative"},
		{"negative deadline", acme("    runner: { activeDeadlineSeconds: -5 }\n"), "must not be negative"},
		{"negative retention", "retention:\n  disabledIndexGrace: -1h\n" + minimal, "retention.disabledIndexGrace"},
		{"negative settle default", "defaults:\n  settle: -1s\n" + minimal, "defaults.settle must not be negative"},
		{"unknown feedback", "defaults:\n  review: { feedback: exhaustive }\n" + minimal, "defaults.review.feedback must be detailed, standard or minimal"},
		{"context without a description", "defaults:\n  review: { context: [{ path: db/schema.sql }] }\n" + minimal, "defaults.review.context[0]: description is required"},
		{"context outside the repository", "defaults:\n  review: { context: [{ path: ../x, description: x }] }\n" + minimal, "escapes the repository"},
		{"context with a bad glob", "defaults:\n  review: { context: [{ path: x, description: x, paths: ['['] }] }\n" + minimal, "paths[0] \"[\" is not a valid glob"},
		{"rule with a bad id", "defaults:\n  review: { rules: [{ id: Wrap_Errors, rule: x }] }\n" + minimal, `defaults.review.rules[0].id "Wrap_Errors" must be`},
		{"rule listed twice", acme("    review: { rules: [{ id: a, rule: x }, { id: a, rule: y }] }\n"), `review.rules[1].id "a" is listed twice`},
		{"blank rule", acme("    repositories: [{ name: x, review: { rules: [{ id: a, rule: ' ' }] } }]\n"), "review.rules[0]: set one of rule or file"},
		{"overlong rule", "defaults:\n  review: { rules: [{ id: a, rule: " + strings.Repeat("x", MaxRuleChars+1) + " }] }\n" + minimal, "over the 2000 allowed"},
		{"rule with a bad glob", "defaults:\n  review: { rules: [{ id: a, rule: x, paths: ['['] }] }\n" + minimal, `rules[0].paths[0] "[" is not a valid glob`},
		{"negative settle account", acme("    settle: -1s\n"), "must not be negative"},
		{"negative settle repository", acme("    repositories: [{ name: x, settle: -1s }]\n"), "must not be negative"},
		{"indexing role removed", "defaults:\n  models:\n    indexing: p/m\n" + minimal, "field indexing not found"},
		{"bad ignore glob", acme("    repositories: [{ name: x, ignore: ['['] }]\n"), "not a valid glob"},
		{"filter syntax error", "defaults:\n  filter: 'pr.draft &&'\n" + minimal, "defaults.filter"},
		{"filter fails smoke test", "defaults:\n  filter: 'pr.labels[5].name == \"x\"'\n" + minimal, "smoke test"},
		{"repository filter error", acme("    repositories: [{ name: x, filter: 'pr.title' }]\n"), "repositories[0].filter"},
		{"repository with its owner", acme("    repositories: [{ name: acme/x }]\n"), "without its owner"},
		{"duplicate repository", acme("    repositories: [{ name: x }, { name: x }]\n"), "duplicates repositories[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadBytes(t, []byte(tt.yaml))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestJobTimeoutBounds checks that runner.activeDeadlineSeconds and
// agent.timeout are accepted up to the point where River's job timeout cap
// would otherwise cut the runner or the review short, and rejected past it.
func TestJobTimeoutBounds(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")

	withRepo := func(repo string) string {
		return acme("    repositories: [" + repo + "]\n")
	}
	withDeadline := func(seconds int64) string {
		return acme(fmt.Sprintf("    runner: { activeDeadlineSeconds: %d }\n", seconds))
	}

	tests := []struct {
		name string
		yaml string
		want string // substring of the error; empty means the config must be accepted
	}{
		{"runner deadline at the cap", withDeadline(int64(jobtimeout.MaxRunnerDeadline.Seconds())), ""},
		{"runner deadline past the cap", withDeadline(int64(jobtimeout.MaxRunnerDeadline.Seconds()) + 1), "runner.activeDeadlineSeconds must not exceed"},
		{"agent timeout at the cap", withRepo(fmt.Sprintf("{ name: x, agent: { timeout: %ds } }", int64(jobtimeout.MaxAgentTimeout.Seconds()))), ""},
		{"agent timeout past the cap", withRepo(fmt.Sprintf("{ name: x, agent: { timeout: %ds } }", int64(jobtimeout.MaxAgentTimeout.Seconds())+1)), "agent.timeout must not exceed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadBytes(t, []byte(tt.yaml))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestWatch(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	path := filepath.Join(t.TempDir(), "config.yaml")
	file := []byte(minimal)
	named := func(name string) string { return strings.Replace(string(file), "name: acme-bot", "name: "+name, 1) }
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(string(file))

	applied := make(chan *File, 4)
	rejected := make(chan error, 1)
	ctx := t.Context()
	go Watch(ctx, path, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(f *File) { applied <- f }, func(err error) {
			select {
			case rejected <- err:
			default:
			}
		})

	expectNone := func(why string) {
		t.Helper()
		select {
		case f := <-applied:
			t.Fatalf("%s: unexpected apply of %d connections", why, len(f.Connections))
		case <-time.After(150 * time.Millisecond):
		}
	}
	expectApply := func(name string) {
		t.Helper()
		select {
		case f := <-applied:
			if f.Connections[0].Name != name {
				t.Fatalf("applied connection %q, want %q", f.Connections[0].Name, name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no apply for %q", name)
		}
	}

	expectNone("unchanged content on the first ticks")
	write(named("acme-two"))
	expectApply("acme-two")
	select {
	case <-rejected: // a tick that caught an earlier write half done
	default:
	}
	write("connections: [{ name: Bad }]\n")
	expectNone("an invalid file must not be applied")
	// A tick can also catch a write half done, so only that the invalid
	// file was reported is certain, not how many times.
	select {
	case <-rejected:
	case <-time.After(2 * time.Second):
		t.Fatal("an invalid file was not reported rejected")
	}
	// Reverting to the content last applied applies it again, so the caller
	// learns the invalid file is gone.
	write(named("acme-two"))
	expectApply("acme-two")
	write(named("acme-three"))
	expectApply("acme-three")
}

// TestPollingIndexingAndRunnerDefaults checks the tuning that lives in the
// file rather than the environment: its defaults, an explicit zero that
// turns polling off, and the account, defaults.runner, built-in order of a
// runner's deadline and resources.
func TestPollingIndexingAndRunnerDefaults(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")

	t.Run("defaults when unset", func(t *testing.T) {
		f, err := loadBytes(t, []byte(minimal))
		if err != nil {
			t.Fatal(err)
		}
		deadline, resources := f.RunnerFor(&f.Accounts[0])
		if f.PollInterval() != DefaultPollInterval || f.PollLookback() != DefaultPollLookback || f.OnboardWindow() != DefaultOnboardWindow ||
			deadline != DefaultRunnerDeadline || resources != nil {
			t.Fatalf("interval=%s lookback=%s window=%d deadline=%s resources=%v",
				f.PollInterval(), f.PollLookback(), f.OnboardWindow(), deadline, resources)
		}
		if d, _ := f.RunnerFor(nil); d != DefaultRunnerDeadline {
			t.Fatalf("RunnerFor(nil) = %s", d)
		}
	})

	t.Run("set values, and interval 0 turns polling off", func(t *testing.T) {
		f, err := loadBytes(t, []byte(`
polling: { interval: 0s, lookback: 1h }
indexing: { onboardWindow: 8 }
defaults:
  runner: { activeDeadlineSeconds: 600, resources: { limits: { memory: 1Gi } } }
`+acme("    runner: { activeDeadlineSeconds: 60 }\n")))
		if err != nil {
			t.Fatal(err)
		}
		if f.PollInterval() != 0 || f.PollLookback() != time.Hour || f.OnboardWindow() != 8 {
			t.Fatalf("interval=%s lookback=%s window=%d", f.PollInterval(), f.PollLookback(), f.OnboardWindow())
		}
		deadline, resources := f.RunnerFor(&f.Accounts[0])
		if deadline != time.Minute || resources["limits"] == nil {
			t.Fatalf("account runner = %s %v; want the account's deadline over the default's resources", deadline, resources)
		}
		if d, _ := f.RunnerFor(nil); d != 10*time.Minute {
			t.Fatalf("RunnerFor(nil) = %s, want defaults.runner's 10m", d)
		}
	})

	refused := map[string]string{
		"polling: { interval: -1m }":      "polling.interval and polling.lookback must not be negative",
		"indexing: { onboardWindow: -1 }": "indexing.onboardWindow must not be negative",
		fmt.Sprintf("defaults: { runner: { activeDeadlineSeconds: %d } }", int64(jobtimeout.MaxRunnerDeadline.Seconds())+1): "defaults.runner.activeDeadlineSeconds must not exceed",
	}
	for block, want := range refused {
		t.Run(block, func(t *testing.T) {
			if _, err := loadBytes(t, []byte(block+"\n"+minimal)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, want)
			}
		})
	}
}

// TestScopePrecedence checks ADR-0010 §2.4: every repository setting can be
// written at the defaults, an account and a repository entry, the narrowest
// one written wins even when it is empty or zero, and ignore globs add up.
func TestScopePrecedence(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	const head = `providers:
  p: { type: openai, apiKey: { env: TEST_WEBHOOK_SECRET } }
defaults:
  models: { review: p/big, fallback: p/small }
  filter: "!pr.draft"
  settle: 2m
  ignore: ["defaults/**"]
  mode: agentic
  agent: { maxSteps: 9 }
  incremental: { maxDeltaFiles: 3 }
  review: { rules: [{ id: ops, file: ops/rules.md }], templates: { summary: ops/summary.tmpl } }
  limits: { tokensPerMonth: 1000, reviewsPerDay: 5 }
`
	account := func(accountKeys, repos string) string {
		return head + acme(""+accountKeys+"    repositories: ["+repos+"]\n")
	}
	parse := func(t *testing.T, doc string) *File {
		t.Helper()
		f, err := loadBytes(t, []byte(doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return f
	}

	t.Run("an account inherits what it leaves out", func(t *testing.T) {
		f := parse(t, account("", "{ name: x }"))
		s := f.Settings(&f.Accounts[0], "acme/x")
		if s.Filter == nil || s.Settle != 2*time.Minute || s.Mode != ReviewAgentic || s.Agent.MaxSteps != 9 ||
			s.Incremental.MaxDeltaFiles != 3 || s.Models.Fallback != "p/small" || s.Limits.TokensPerMonth != 1000 ||
			!reflect.DeepEqual(s.Review.Rules, []Rule{{ID: "ops", File: "ops/rules.md"}}) || s.Review.Templates.Summary != "ops/summary.tmpl" {
			t.Fatalf("inherited settings = %+v", s)
		}
	})

	t.Run("an empty or zero value written at a narrower scope clears", func(t *testing.T) {
		f := parse(t, account(`    filter: ""
    settle: 0s
    models: { fallback: "" }
    limits: { tokensPerMonth: 0 }
`, `{ name: x, review: { templates: { summary: "" } } }`))
		s := f.Settings(&f.Accounts[0], "acme/x")
		if s.Filter != nil || s.Settle != 0 || s.Models.Fallback != "" || s.Models.Review != "p/big" ||
			s.Limits.TokensPerMonth != 0 || s.Limits.ReviewsPerDay != 5 {
			t.Fatalf("cleared settings = %+v", s)
		}
		if s.Review.Templates.Summary != "" {
			t.Fatalf("cleared review = %+v", s.Review)
		}
	})

	t.Run("the narrowest scope written wins, field by field", func(t *testing.T) {
		f := parse(t, account(`    mode: single
    agent: { maxSteps: 7 }
    review: { requireSuggestedFix: true }
    ignore: ["account/**"]
`, `{ name: x, models: { review: p/small }, forks: true, agent: { maxTokens: 500 }, ignore: ["repo/**"] }`))
		s := f.Settings(&f.Accounts[0], "acme/x")
		if s.Mode != ReviewSingle || s.Agent.MaxSteps != 7 || s.Agent.MaxTokens != 500 || s.Models.Review != "p/small" || !s.Forks {
			t.Fatalf("settings = %+v", s)
		}
		if !s.Review.RequireSuggestedFix || !reflect.DeepEqual(s.Review.Rules, []Rule{{ID: "ops", File: "ops/rules.md"}}) {
			t.Fatalf("review = %+v, want the account's strictness over the defaults' rules", s.Review)
		}
		want := append(append([]string(nil), DefaultIgnore...), "defaults/**", "account/**", "repo/**")
		if !slices.Equal(s.Ignore, want) {
			t.Fatalf("ignore = %v, want %v", s.Ignore, want)
		}
	})

	t.Run("an explicit concurrency must be positive", func(t *testing.T) {
		if _, err := loadBytes(t, []byte(account("    limits: { concurrency: 0 }\n", "{ name: x }"))); err == nil ||
			!strings.Contains(err.Error(), "concurrency must be positive") {
			t.Fatalf("Parse = %v", err)
		}
	})
}

// TestReviewPresentation checks every finding goes inline, from a
// detailed review, unless a scope sets another feedback level or turns
// inline comments off.
func TestReviewPresentation(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	parse := func(t *testing.T, accountKeys, repos string) *File {
		t.Helper()
		f, err := loadBytes(t, []byte(acme(""+accountKeys+"    repositories: ["+repos+"]\n")))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return f
	}
	f := parse(t, "", "{ name: x }")
	if s := f.Settings(&f.Accounts[0], ""); !s.Review.InlineComments || s.Review.Feedback != FeedbackDetailed || !s.Review.AgentFiles {
		t.Fatalf("review = %+v, want every finding inline, from a detailed review that reads agent files", s.Review)
	}
	f = parse(t, "    review: { inlineComments: false, feedback: minimal, agentFiles: false }\n",
		"{ name: x, review: { inlineComments: true } }, { name: y, review: { feedback: standard } }")
	if s := f.Settings(&f.Accounts[0], "acme/x"); !s.Review.InlineComments || s.Review.Feedback != FeedbackMinimal || s.Review.AgentFiles {
		t.Fatalf("review = %+v, want the account's feedback and agent files with the repository's inline comments", s.Review)
	}
	if s := f.Settings(&f.Accounts[0], "acme/y"); s.Review.Feedback != FeedbackStandard {
		t.Fatalf("review = %+v, want the repository's feedback over the account's", s.Review)
	}
}

// TestAllow checks the allow block resolves bound by bound like any other
// setting, and that load refuses a bound a repository could not choose
// and an admin value outside its own bounds.
func TestAllow(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	const head = `providers:
  p: { type: openai, apiKey: { env: TEST_WEBHOOK_SECRET } }
defaults:
  models: { review: p/big }
  allow:
    modes: [single, agentic]
    models: [p/big, p/small]
    commands: [rg, fd]
    agent: { maxSteps: 60, timeout: 20m }
    settle: 30m
`
	doc := func(accountKeys, repos string) string {
		return head + acme(""+accountKeys+"    repositories: ["+repos+"]\n")
	}

	f, err := loadBytes(t, []byte(doc("    allow: { models: [p/big] }\n", "{ name: x, allow: { settle: 5m, commands: [] } }")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := f.Settings(&f.Accounts[0], "acme/x")
	a := s.Allow
	if !slices.Equal(a.Modes, []ReviewMode{ReviewSingle, ReviewAgentic}) || !slices.Equal(a.Models, []ModelRef{"p/big"}) ||
		a.Commands == nil || len(a.Commands) != 0 || *a.Agent.MaxSteps != 60 || *a.Agent.Timeout != 20*time.Minute ||
		a.Agent.MaxTokens != nil || *a.Settle != 5*time.Minute {
		t.Fatalf("allow = %+v", a)
	}
	if account := f.Settings(&f.Accounts[0], ""); *account.Allow.Settle != 30*time.Minute || !slices.Equal(account.Allow.Commands, []string{"rg", "fd"}) {
		t.Fatalf("account allow = %+v", account.Allow)
	}

	tests := []struct {
		name, yaml, want string
	}{
		{"an unknown mode", doc("    allow: { modes: [turbo] }\n", ""), "accounts[0].allow.modes[0] must be single or agentic"},
		{"a model of an undeclared provider", doc("    allow: { models: [q/big] }\n", ""), "allow.models[0] references provider \"q\""},
		{"a command path", doc("", "{ name: x, allow: { commands: [/bin/sh] } }"), "allow.commands[0] \"/bin/sh\" must be a bare command name"},
		{"a bound that is not positive", doc("    allow: { agent: { maxTokens: 0 } }\n", ""), "allow.agent bounds must be positive"},
		{"a negative settle bound", doc("    allow: { settle: -1s }\n", ""), "allow.settle must not be negative"},
		{"the admin's mode outside its bounds", doc("    mode: agentic\n    allow: { modes: [single] }\n", ""), "mode agentic is outside allow.modes"},
		{"the admin's model outside its bounds", doc("    models: { fallback: p/tiny }\n", ""), "models.fallback \"p/tiny\" is outside allow.models"},
		{"a repository's command outside its bounds", doc("", "{ name: x, agent: { commands: [curl] } }"), "agent.commands \"curl\" is outside allow.commands"},
		{"a built-in limit above its bound", doc("    allow: { agent: { maxSteps: 10 } }\n", ""), "agent.maxSteps is above allow.agent.maxSteps"},
		{"the admin's settle above its bound", doc("    settle: 1h\n", ""), "settle is above allow.settle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadBytes(t, []byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

// TestTools checks the tool catalog: what a run allowed some commands
// mounts, and the names, images, paths and commands load refuses.
func TestTools(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f, err := loadBytes(t, []byte(`
tools:
  - { name: helm, image: registry.example/helm:3, path: /usr/bin }
  - { name: flux-tools, image: registry.example/flux:2, commands: [flux, flate] }
`+minimal))
	if err != nil {
		t.Fatal(err)
	}
	names := func(ts []Tool) []string {
		out := make([]string, 0, len(ts))
		for _, x := range ts {
			out = append(out, x.Name)
		}
		return out
	}
	if got := names(f.ToolsFor([]string{"curl", "flate"})); !slices.Equal(got, []string{"flux-tools"}) {
		t.Fatalf("ToolsFor(curl, flate) = %v, want flux-tools", got)
	}
	if got := names(f.ToolsFor([]string{"helm", "flux"})); !slices.Equal(got, []string{"helm", "flux-tools"}) {
		t.Fatalf("ToolsFor(helm, flux) = %v", got)
	}
	if got := f.ToolsFor([]string{"curl", "rg"}); got != nil {
		t.Fatalf("ToolsFor(curl, rg) = %v, want nil: the runner image provides those", got)
	}

	refused := map[string]string{
		"{ name: Helm, image: x }":                                              "must be lowercase",
		"{ name: helm, image: x }, { name: helm, image: y }":                    `"helm" is listed twice`,
		"{ name: helm }":                                                        "tools[0].image is required",
		"{ name: helm, image: x, path: usr/bin }":                               "must be a clean absolute path",
		"{ name: helm, image: x, path: /usr/../etc }":                           "must be a clean absolute path",
		"{ name: helm, image: x, commands: [bin/helm] }":                        "must be a bare command name",
		"{ name: helm, image: x }, { name: helm2, image: y, commands: [helm] }": `which tool "helm" already provides`,
	}
	for list, want := range refused {
		t.Run(list, func(t *testing.T) {
			if _, err := loadBytes(t, []byte("tools: ["+list+"]\n"+minimal)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, want)
			}
		})
	}
}

// TestRulesAddUp: each scope's rules follow the broader scope's, and one
// with an id already listed replaces that rule where it stands.
func TestRulesAddUp(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f := mustLoad(t, "defaults:\n  review: { rules: [{ id: a, rule: A }, { id: b, rule: B }] }\n"+
		acme("    review: { rules: [{ id: c, rule: C }] }\n    repositories: [{ name: x, review: { rules: [{ id: a, rule: A2, paths: ['**/*.go'] }] } }]\n"))
	for repo, want := range map[string][]Rule{
		"acme/x":        {{ID: "a", Rule: "A2", Paths: []string{"**/*.go"}}, {ID: "b", Rule: "B"}, {ID: "c", Rule: "C"}},
		"acme/unlisted": {{ID: "a", Rule: "A"}, {ID: "b", Rule: "B"}, {ID: "c", Rule: "C"}},
	} {
		if got := f.Settings(&f.Accounts[0], repo).Review.Rules; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rules = %+v, want %+v", repo, got, want)
		}
	}
	for repo, want := range map[string]map[string]Scope{
		"acme/x":        {"a": ScopeRepository, "b": ScopeDefaults, "c": ScopeAccount},
		"acme/unlisted": {"a": ScopeDefaults, "b": ScopeDefaults, "c": ScopeAccount},
	} {
		if got := f.RuleScopes(&f.Accounts[0], repo); !maps.Equal(got, want) {
			t.Errorf("%s: scopes = %v, want %v", repo, got, want)
		}
	}
}

func TestRepositoryModeAgentReview(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	withRepo := func(repo string) string {
		return acme("    repositories: [" + repo + "]\n")
	}

	t.Run("defaults resolve when unset", func(t *testing.T) {
		f, err := loadBytes(t, []byte(withRepo("{ name: x }")))
		if err != nil {
			t.Fatal(err)
		}
		for _, repo := range []string{"acme/x", "acme/unlisted"} {
			s := f.Settings(&f.Accounts[0], repo)
			if s.Mode != ReviewAgentic || !reflect.DeepEqual(s.Agent, DefaultAgent) || s.Incremental.MaxDeltaFiles != DefaultMaxDeltaFiles {
				t.Fatalf("%s: mode=%q agent=%+v incremental=%+v", repo, s.Mode, s.Agent, s.Incremental)
			}
			if s.Review.RequireSuggestedFix || len(s.Review.Rules) != 0 || s.Review.Templates != (ReviewTemplates{}) {
				t.Fatalf("%s: review = %+v", repo, s.Review)
			}
		}
		if DefaultMaxDeltaFiles != 25 {
			t.Fatalf("DefaultMaxDeltaFiles = %d", DefaultMaxDeltaFiles)
		}
	})

	t.Run("repository values override the defaults", func(t *testing.T) {
		f, err := loadBytes(t, []byte(withRepo(`{ name: x, mode: agentic,
      agent: { maxSteps: 12, maxToolOutputBytes: 4096, maxTokens: 250000, timeout: 3m, commands: [curl, rg], commandTimeout: 10s },
      incremental: { maxDeltaFiles: 5 },
      review: { rules: [{ id: style, file: docs/rules.md }], requireSuggestedFix: true,
        templates: { summary: .kritik/summary.md.tmpl, inline: .kritik/inline.md.tmpl } } }`)))
		if err != nil {
			t.Fatal(err)
		}
		s := f.Settings(&f.Accounts[0], "acme/x")
		want := AgentSettings{MaxSteps: 12, MaxToolOutputBytes: 4096, MaxTokens: 250_000, Timeout: 3 * time.Minute,
			Commands: []string{"curl", "rg"}, CommandTimeout: 10 * time.Second}
		if s.Mode != ReviewAgentic || !reflect.DeepEqual(s.Agent, want) || s.Incremental.MaxDeltaFiles != 5 {
			t.Fatalf("mode=%q agent=%+v incremental=%+v", s.Mode, s.Agent, s.Incremental)
		}
		if !s.Review.RequireSuggestedFix || len(s.Review.Rules) != 1 || s.Review.Rules[0].File != "docs/rules.md" ||
			s.Review.Templates.Summary != ".kritik/summary.md.tmpl" || s.Review.Templates.Inline != ".kritik/inline.md.tmpl" {
			t.Fatalf("review = %+v", s.Review)
		}
		if got := s.Review.Referenced(); strings.Join(got, ",") != "docs/rules.md,.kritik/summary.md.tmpl,.kritik/inline.md.tmpl" {
			t.Fatalf("referenced = %v", got)
		}
	})

	t.Run("a partial agent block keeps the other defaults", func(t *testing.T) {
		f, err := loadBytes(t, []byte(withRepo("{ name: x, agent: { maxSteps: 7 } }")))
		if err != nil {
			t.Fatal(err)
		}
		want := DefaultAgent
		want.MaxSteps = 7
		if got := f.Settings(&f.Accounts[0], "acme/x").Agent; !reflect.DeepEqual(got, want) {
			t.Fatalf("agent = %+v, want %+v", got, want)
		}
	})

	t.Run("review modes", func(t *testing.T) {
		for m, valid := range map[ReviewMode]bool{ReviewSingle: true, ReviewAgentic: true, "": false, "loop": false} {
			if m.Valid() != valid {
				t.Fatalf("%q.Valid() = %v", m, !valid)
			}
		}
	})

	rejects := []struct{ name, repo, want string }{
		{"invalid mode", "{ name: x, mode: loop }", "mode must be single or agentic"},
		{"zero max steps", "{ name: x, agent: { maxSteps: 0 } }", "agent.maxSteps must be positive"},
		{"negative max steps", "{ name: x, agent: { maxSteps: -1 } }", "agent.maxSteps must be positive"},
		{"zero tool output", "{ name: x, agent: { maxToolOutputBytes: 0 } }", "agent.maxToolOutputBytes must be positive"},
		{"zero max tokens", "{ name: x, agent: { maxTokens: 0 } }", "agent.maxTokens must be positive"},
		{"negative max tokens", "{ name: x, agent: { maxTokens: -5 } }", "agent.maxTokens must be positive"},
		{"zero timeout", "{ name: x, agent: { timeout: 0s } }", "agent.timeout must be positive"},
		{"zero delta files", "{ name: x, incremental: { maxDeltaFiles: 0 } }", "incremental.maxDeltaFiles must be positive"},
		{"negative delta files", "{ name: x, incremental: { maxDeltaFiles: -3 } }", "incremental.maxDeltaFiles must be positive"},
		{"unknown agent key", "{ name: x, agent: { steps: 3 } }", "field steps not found"},
		{"zero command timeout", "{ name: x, agent: { commandTimeout: 0s } }", "agent.commandTimeout must be at least 1s"},
		{"sub-second command timeout", "{ name: x, agent: { commandTimeout: 500ms } }", "agent.commandTimeout must be at least 1s"},
		{"command path", "{ name: x, agent: { commands: [/usr/bin/curl] } }", "must be a bare command name"},
		{"relative command path", "{ name: x, agent: { commands: [./tool] } }", "must be a bare command name"},
		{"empty command", "{ name: x, agent: { commands: [''] } }", "must be a bare command name"},
		{"duplicate command", "{ name: x, agent: { commands: [rg, rg] } }", "is listed twice"},
		{"absolute rule file", "{ name: x, review: { rules: [{ id: a, file: /etc/passwd }] } }", "must be relative"},
		{"escaping template path", "{ name: x, review: { templates: { summary: ../x.tmpl } } }", "escapes the repository"},
		{"a rule with a file and text", "{ name: x, review: { rules: [{ id: a, rule: Check., file: a.md }] } }", "set one of rule or file"},
	}
	for _, tt := range rejects {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := loadBytes(t, []byte(withRepo(tt.repo)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestRetentionTranscripts(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	tests := []struct {
		name string
		yaml string
		want time.Duration
		err  string
	}{
		{"default", "", 30 * 24 * time.Hour, ""},
		{"set", "retention:\n  transcripts: 48h\n", 48 * time.Hour, ""},
		{"too short", "retention:\n  transcripts: 1h\n", 0, "retention.transcripts"},
		{"negative", "retention:\n  transcripts: -48h\n", 0, "retention.transcripts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := loadBytes(t, []byte(tt.yaml+minimal))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error %v does not mention %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Retention.TranscriptsOrDefault(); got != tt.want {
				t.Fatalf("transcripts = %s, want %s", got, tt.want)
			}
		})
	}
}
