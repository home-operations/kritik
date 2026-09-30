package configfile

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/model"
)

// fixture sets the variables testdata/full.yaml's secrets name and returns
// its content.
func fixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("TEST_LOCAL_KEY", "sk-ant-test\n")
	t.Setenv("TEST_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_CLIENT_ID", "Iv1.fromenv")
	return string(raw)
}

func TestLoadFull(t *testing.T) {
	f, err := load(t, fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Run("providers resolve from env", func(t *testing.T) {
		if got := f.Providers["openrouter"].APIKeyValue().Value(); got != "sk-or-test" {
			t.Fatalf("openrouter key = %q", got)
		}
		if got := f.Providers["local"].APIKeyValue().Value(); got != "sk-ant-test" {
			t.Fatalf("local key = %q (trailing newline must be trimmed)", got)
		}
	})

	t.Run("records the variables its secrets came from", func(t *testing.T) {
		want := []string{"TEST_CLIENT_ID", "TEST_LOCAL_KEY", "TEST_OPENROUTER_API_KEY", "TEST_PRIVATE_KEY", "TEST_WEBHOOK_SECRET"}
		if got := f.SecretEnv(); !slices.Equal(got, want) {
			t.Fatalf("SecretEnv = %q, want %q", got, want)
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

func TestIgnore(t *testing.T) {
	f, err := load(t, fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
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

// githubMinimal is the smallest configuration: one app serving acme;
// clientFields is spliced into the app's entry.
func githubMinimal(clientFields string) string {
	return `
apps:
  - { name: acme-bot, accounts: [acme], ` + clientFields + `privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }
`
}

// acme is minimal with entries, lines of the repositories map, such as
// "  acme/*: { mode: agentic }\n".
func acme(entries string) string {
	if entries == "" {
		return minimal
	}
	return minimal + "repositories:\n" + entries
}

// acmeAccount is minimal with keys, lines of acme's accounts entry.
func acmeAccount(keys string) string { return minimal + "accounts:\n  acme:\n" + keys }

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
		{"on unless turned off", acme("  acme/listed: { mode: single }\n"),
			map[string]bool{"acme/new": true, "acme/listed": true}},
		{"off at the defaults", "defaults: { enabled: false }\n" + acme("  acme/listed: {}\n"),
			map[string]bool{"acme/new": false, "acme/listed": false}},
		{"owner/* over the defaults", "defaults: { enabled: false }\n" + acme("  acme/*: { enabled: true }\n"),
			map[string]bool{"acme/new": true}},
		{"off at owner/*", acme("  acme/*: { enabled: false }\n  acme/listed: {}\n"),
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
		{"a source repository turned off", acme("  acme/*: { enabled: false }\n"), "acme/app", RepoTraits{}, false},
		{"a fork", acme(""), "acme/copy", fork, false},
		{"a fork owner/* turns on", acme("  acme/*: { enabled: true }\n"), "acme/copy", fork, false},
		{"a fork with an entry", acme("  acme/copy: { mode: agentic }\n"), "acme/copy", fork, false},
		{"an archived repository", acme(""), "acme/old", archived, false},
		{"a repository an admin turned off", acme("  acme/*: { enabled: true }\n"), "acme/app", RepoTraits{TurnedOn: new(false)}, false},
		{"a repository an admin turned on", acme("  acme/*: { enabled: false }\n"), "acme/app", RepoTraits{TurnedOn: new(true)}, true},
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
	withOwn := minimal + "accounts:\n  acme:\n    providers:\n" +
		"      own: { type: openai, baseUrl: http://llm.internal:4000/v1, apiKey: { env: TEST_WEBHOOK_SECRET } }\n" +
		"repositories:\n  acme/*: { models: { review: own/big } }\n"
	f, err := loadBytes(t, []byte(withOwn))
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
		{"a name a model reference cannot carry", strings.Replace(withOwn, "      own:", "      Own:", 1), "a provider name must be lowercase"},
		{"a name the instance already uses", "providers:\n  own: { type: anthropic, apiKey: { env: TEST_WEBHOOK_SECRET } }\n" + withOwn,
			"the instance declares a provider by that name"},
		{"the defaults naming an account's provider", "defaults:\n  models: { review: own/big }\n" + withOwn, "not declared under providers"},
		{"an invalid provider", strings.Replace(withOwn, "type: openai", "type: gemini", 1), "accounts.acme.providers.own.type must be"},
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
		{"unknown nested key", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme], owner: acme", 1), "field owner not found"},
		{"a forge key", strings.Replace(minimal, "name: acme-bot, ", "name: acme-bot, forge: github, ", 1), "field forge not found"},
		{"bad app name", strings.Replace(minimal, "name: acme-bot", "name: Acme Bot", 1), "lowercase"},
		{"duplicate app", minimal + "  - { name: acme-bot, accounts: [other], clientId: x, " +
			"privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }\n", "names are hook paths"},
		{"an account two apps serve", minimal + "  - { name: acme-two, accounts: [ACME], clientId: x, " +
			"privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }\n", "an account is served by one app"},
		{"duplicate account entry", minimal + "accounts:\n  acme: {}\n  Acme: {}\n", "duplicates accounts.Acme"},
		{"an account entry with its owner", minimal + "accounts:\n  acme/x: {}\n", "must be the account's name"},
		{"an account entry with an unknown key", minimal + "accounts:\n  acme: { mode: single }\n", "field mode not found"},
		{"missing accounts", strings.Replace(minimal, "accounts: [acme], ", "", 1), "accounts must list at least one account"},
		{"blank account", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme, ' ']", 1), `accounts[1] " " must be the account's name`},
		{"a served account with its owner", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme/x]", 1), `accounts[0] "acme/x" must be the account's name`},
		{"account listed twice", strings.Replace(minimal, "accounts: [acme]", "accounts: [acme, ACME]", 1), `accounts[1] "ACME" is listed twice`},
		{"an app without a client id", githubMinimal(""), "apps[0].clientId is required"},
		{"a client id reference with an unknown key", githubMinimal("clientId: { vault: x }, "), "field vault not found"},
		{"missing private key", strings.Replace(minimal, "privateKey: { env: TEST_PRIVATE_KEY }, ", "", 1), "apps[0].privateKey: reference must set env"},
		{"unset env reference", strings.Replace(minimal, "TEST_PRIVATE_KEY", "TEST_DOES_NOT_EXIST", 1), "is not set"},
		{"empty env reference", strings.Replace(minimal, "TEST_PRIVATE_KEY", "TEST_EMPTY", 1), "privateKey is required"},
		{"a file reference", strings.Replace(minimal, "{ env: TEST_PRIVATE_KEY }", "{ file: /var/run/secrets/token }", 1), "field file not found"},
		{"empty reference", strings.Replace(minimal, "{ env: TEST_PRIVATE_KEY }", "{}", 1), "apps[0].privateKey: reference must set env"},
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
		{"owner/* model referencing undeclared provider", acme("  acme/*: { models: { review: nope/gpt } }\n"), "not declared under providers"},
		{"negative limit", "defaults:\n  limits:\n    reviewsPerDay: -1\n" + minimal, "must not be negative"},
		{"negative account limit", acmeAccount("    limits: { tokensPerMonth: -1 }\n"), "accounts.acme.limits: limits must not be negative"},
		{"negative settle default", "defaults:\n  settle: -1s\n" + minimal, "defaults.settle must not be negative"},
		{"unknown feedback", "defaults:\n  feedback: exhaustive\n" + minimal, "defaults.feedback must be detailed, standard or minimal"},
		{"context without a description", "defaults:\n  context: [{ path: db/schema.sql }]\n" + minimal, "defaults.context[0]: description is required"},
		{"context outside the repository", "defaults:\n  context: [{ path: ../x, description: x }]\n" + minimal, "escapes the repository"},
		{"context with a bad glob", "defaults:\n  context: [{ path: x, description: x, paths: ['['] }]\n" + minimal, "paths[0] \"[\" is not a valid glob"},
		{"rule with a bad id", "defaults:\n  rules: [{ id: Wrap_Errors, rule: x }]\n" + minimal, `defaults.rules[0].id "Wrap_Errors" must be`},
		{"rule listed twice", acme("  acme/*: { rules: [{ id: a, rule: x }, { id: a, rule: y }] }\n"), `repositories.acme/*.rules[1].id "a" is listed twice`},
		{"blank rule", acme("  acme/x: { rules: [{ id: a, rule: ' ' }] }\n"), "repositories.acme/x.rules[0]: set one of rule or file"},
		{"overlong rule", "defaults:\n  rules: [{ id: a, rule: " + strings.Repeat("x", MaxRuleChars+1) + " }]\n" + minimal, "over the 2000 allowed"},
		{"rule with a bad glob", "defaults:\n  rules: [{ id: a, rule: x, paths: ['['] }]\n" + minimal, `rules[0].paths[0] "[" is not a valid glob`},
		{"rule whenExpr syntax error", acme("  acme/x: { rules: [{ id: a, rule: x, whenExpr: 'pr.draft &&' }] }\n"), "repositories.acme/x.rules[0].whenExpr"},
		{"rule whenExpr not a bool", "defaults:\n  rules: [{ id: a, rule: x, whenExpr: pr.title }]\n" + minimal, "defaults.rules[0].whenExpr"},
		{"negative settle at owner/*", acme("  acme/*: { settle: -1s }\n"), "repositories.acme/*.settle must not be negative"},
		{"negative settle repository", acme("  acme/x: { settle: -1s }\n"), "repositories.acme/x.settle must not be negative"},
		{"indexing role removed", "defaults:\n  models:\n    indexing: p/m\n" + minimal, "field indexing not found"},
		{"bad ignore glob", acme("  acme/x: { ignore: ['['] }\n"), "not a valid glob"},
		{"filter syntax error", "defaults:\n  filterExpr: 'pr.draft &&'\n" + minimal, "defaults.filterExpr"},
		{"filter fails smoke test", "defaults:\n  filterExpr: 'pr.labels[5].name == \"x\"'\n" + minimal, "smoke test"},
		{"repository filter error", acme("  acme/x: { filterExpr: 'pr.title' }\n"), "repositories.acme/x.filterExpr"},
		{"a repository key without an owner", acme("  x: {}\n"), "keyed owner/* or owner/name"},
		{"a repository key too deep", acme("  acme/x/y: {}\n"), "keyed owner/* or owner/name"},
		{"duplicate repository", acme("  acme/x: {}\n  ACME/x: {}\n"), "duplicates repositories.ACME/x"},
		{"a repository entry that says where it starts", acme("  acme/x: { enabled: false }\n"), "repositories.acme/x.enabled: turn a repository on or off in the dashboard"},
		{"a file key that moved to the environment", "polling: { interval: 5m }\n" + minimal, "field polling not found"},
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

	withTimeout := func(seconds int64) string {
		return acme(fmt.Sprintf("  acme/x: { agent: { timeout: %ds } }\n", seconds))
	}

	tests := []struct {
		name string
		yaml string
		want string // substring of the error; empty means the config must be accepted
	}{
		{"agent timeout at the cap", withTimeout(int64(jobtimeout.MaxAgentTimeout.Seconds())), ""},
		{"agent timeout past the cap", withTimeout(int64(jobtimeout.MaxAgentTimeout.Seconds()) + 1), "agent.timeout must not exceed"},
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

func TestScopePrecedence(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	const head = `providers:
  p: { type: openai, apiKey: { env: TEST_WEBHOOK_SECRET } }
defaults:
  models: { review: p/big, fallback: p/small }
  filterExpr: "!pr.draft"
  settle: 2m
  ignore: ["defaults/**"]
  mode: agentic
  agent: { maxSteps: 9 }
  incremental: { maxDeltaFiles: 3 }
  rules: [{ id: ops, file: ops/rules.md }]
  comments: { summaryTemplate: ops/summary.tmpl }
  limits: { tokensPerMonth: 1000, reviewsPerDay: 5 }
`
	// doc gives acme's owner/* and acme/x entries the flow keys owner and
	// repo, and its accounts entry limits when there are any.
	doc := func(limits, owner, repo string) string {
		out := head + acme("  acme/*: { "+owner+" }\n  acme/x: { "+repo+" }\n")
		if limits != "" {
			out += "accounts:\n  acme: { limits: { " + limits + " } }\n"
		}
		return out
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
		f := parse(t, doc("", "", ""))
		s := f.Settings(&f.Accounts[0], "acme/x")
		if s.Filter == nil || s.Settle != 2*time.Minute || s.Mode != ReviewAgentic || s.Agent.MaxSteps != 9 ||
			s.Incremental.MaxDeltaFiles != 3 || s.Models.Fallback != "p/small" || s.Limits.TokensPerMonth != 1000 ||
			!reflect.DeepEqual(s.Review.Rules, []Rule{{ID: "ops", File: "ops/rules.md"}}) || s.Review.Templates.Summary != "ops/summary.tmpl" {
			t.Fatalf("inherited settings = %+v", s)
		}
	})

	t.Run("an empty or zero value written at a narrower scope clears", func(t *testing.T) {
		f := parse(t, doc("tokensPerMonth: 0", `filterExpr: "", settle: 0s, models: { fallback: "" }`, `comments: { summaryTemplate: "" }`))
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
		f := parse(t, doc("", `mode: single, agent: { maxSteps: 7 }, requireSuggestedFix: true, ignore: ["account/**"]`,
			`models: { review: p/small }, forks: true, agent: { maxTokens: 500 }, ignore: ["repo/**"]`))
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
		if _, err := loadBytes(t, []byte(doc("concurrency: 0", "", ""))); err == nil ||
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
	parse := func(t *testing.T, owner, repos string) *File {
		t.Helper()
		f, err := loadBytes(t, []byte(acme("  acme/*: { "+owner+" }\n"+repos)))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return f
	}
	f := parse(t, "", "  acme/x: {}\n")
	if s := f.Settings(&f.Accounts[0], ""); !s.Review.InlineComments || s.Review.Feedback != FeedbackDetailed || !s.Review.AgentFiles {
		t.Fatalf("review = %+v, want every finding inline, from a detailed review that reads agent files", s.Review)
	}
	f = parse(t, "comments: { inline: false }, feedback: minimal, agentFiles: false",
		"  acme/x: { comments: { inline: true } }\n  acme/y: { feedback: standard }\n")
	if s := f.Settings(&f.Accounts[0], "acme/x"); !s.Review.InlineComments || s.Review.Feedback != FeedbackMinimal || s.Review.AgentFiles {
		t.Fatalf("review = %+v, want the account's feedback and agent files with the repository's inline comments", s.Review)
	}
	if s := f.Settings(&f.Accounts[0], "acme/y"); s.Review.Feedback != FeedbackStandard {
		t.Fatalf("review = %+v, want the repository's feedback over the account's", s.Review)
	}
}

// TestSettingsProviders: a repository's settings name the providers its
// account may use, the instance's and its own, sorted.
func TestSettingsProviders(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f := mustLoad(t, "providers:\n  p: { type: openai, apiKey: { env: TEST_WEBHOOK_SECRET } }\n"+
		acmeAccount("    providers:\n      own: { type: openai, apiKey: { env: TEST_WEBHOOK_SECRET } }\n"))
	if got := f.Settings(&f.Accounts[0], "acme/x").Providers; !slices.Equal(got, []string{"own", "p"}) {
		t.Fatalf("providers = %q, want [own p]", got)
	}
}

// TestRulesAddUp: each scope's rules follow the broader scope's, and one
// with an id already listed replaces that rule where it stands.
func TestRulesAddUp(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f := mustLoad(t, "defaults:\n  rules: [{ id: a, rule: A }, { id: b, rule: B }]\n"+
		acme("  acme/*: { rules: [{ id: c, rule: C }] }\n  acme/x: { rules: [{ id: a, rule: A2, paths: ['**/*.go'] }] }\n"))
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
		return acme("  acme/x: " + repo + "\n")
	}

	t.Run("defaults resolve when unset", func(t *testing.T) {
		f, err := loadBytes(t, []byte(withRepo("{}")))
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
		f, err := loadBytes(t, []byte(withRepo(`{ mode: agentic,
      agent: { maxSteps: 12, maxToolOutputBytes: 4096, maxTokens: 250000, timeout: 3m, commands: [curl, rg], commandTimeout: 10s },
      incremental: { maxDeltaFiles: 5 },
      rules: [{ id: style, file: docs/rules.md }], requireSuggestedFix: true,
      comments: { summaryTemplate: .kritik/summary.md.tmpl, inlineTemplate: .kritik/inline.md.tmpl } }`)))
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
		f, err := loadBytes(t, []byte(withRepo("{ agent: { maxSteps: 7 } }")))
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
		{"invalid mode", "{ mode: loop }", "mode must be single or agentic"},
		{"zero max steps", "{ agent: { maxSteps: 0 } }", "agent.maxSteps must be positive"},
		{"negative max steps", "{ agent: { maxSteps: -1 } }", "agent.maxSteps must be positive"},
		{"zero tool output", "{ agent: { maxToolOutputBytes: 0 } }", "agent.maxToolOutputBytes must be positive"},
		{"zero max tokens", "{ agent: { maxTokens: 0 } }", "agent.maxTokens must be positive"},
		{"negative max tokens", "{ agent: { maxTokens: -5 } }", "agent.maxTokens must be positive"},
		{"zero timeout", "{ agent: { timeout: 0s } }", "agent.timeout must be positive"},
		{"zero delta files", "{ incremental: { maxDeltaFiles: 0 } }", "incremental.maxDeltaFiles must be positive"},
		{"negative delta files", "{ incremental: { maxDeltaFiles: -3 } }", "incremental.maxDeltaFiles must be positive"},
		{"unknown agent key", "{ agent: { steps: 3 } }", "field steps not found"},
		{"zero command timeout", "{ agent: { commandTimeout: 0s } }", "agent.commandTimeout must be at least 1s"},
		{"sub-second command timeout", "{ agent: { commandTimeout: 500ms } }", "agent.commandTimeout must be at least 1s"},
		{"command path", "{ agent: { commands: [/usr/bin/curl] } }", "must be a bare command name"},
		{"relative command path", "{ agent: { commands: [./tool] } }", "must be a bare command name"},
		{"empty command", "{ agent: { commands: [''] } }", "must be a bare command name"},
		{"duplicate command", "{ agent: { commands: [rg, rg] } }", "is listed twice"},
		{"absolute rule file", "{ rules: [{ id: a, file: /etc/passwd }] }", "must be relative"},
		{"escaping template path", "{ comments: { summaryTemplate: ../x.tmpl } }", "escapes the repository"},
		{"a rule with a file and text", "{ rules: [{ id: a, rule: Check., file: a.md }] }", "set one of rule or file"},
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
