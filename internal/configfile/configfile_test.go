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
// into a temp dir and its env references set, and returns the file's path.
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
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), "__DIR__", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_FORGEJO_TOKEN", "fj-token")
	t.Setenv("TEST_CLIENT_ID", "Iv1.fromenv")
	return path
}

func TestLoadFull(t *testing.T) {
	f, err := Load(fixture(t))
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

	t.Run("settings layer defaults, tenant, repository", func(t *testing.T) {
		ho, _ := f.Tenant("home-operations")
		od, _ := f.Tenant("onedr0p")
		tests := []struct {
			name     string
			tenant   *Tenant
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
				name: "unlisted repo inherits tenant", tenant: ho, repo: "home-operations/other",
				enabled: true, review: "openrouter/openai/gpt-6-sol", forks: false, conc: 3, perDay: 200,
				filterOK: SamplePR(), filterNo: with(SamplePR(), "draft", true),
			},
			{
				name: "listed repo applies its own settle", tenant: ho, repo: "home-operations/flate",
				enabled: true, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200,
				settle:   30 * time.Second,
				filterOK: SamplePR(),
			},
			{
				name: "repo filter replaces tenant filter", tenant: ho, repo: "home-operations/kopiur",
				enabled: true, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200,
				filterOK: SamplePR(), filterNo: with(SamplePR(), "labels", []any{map[string]any{"name": "skip-review", "color": "0"}}),
			},
			{
				name: "disabled repo", tenant: ho, repo: "home-operations/charts-mirror",
				enabled: false, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200,
			},
			{
				name: "tenant overrides review model and forks", tenant: od, repo: "onedr0p/home-ops",
				enabled: true, review: "local/claude-opus-5", forks: true, conc: 3,
				settle:   2 * time.Minute,
				filterOK: SamplePR(),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := f.Settings(tt.tenant, tt.tenant.Installations[0].Name, tt.repo)
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
		g := &File{Tenants: []Tenant{{Slug: "x"}}}
		if got := g.Settings(&g.Tenants[0], "", "x/y").Limits.Concurrency; got != DefaultConcurrency {
			t.Fatalf("concurrency = %d, want %d", got, DefaultConcurrency)
		}
	})
}

func TestInstallationCredentials(t *testing.T) {
	f, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in, tenant, ok := f.Installation("sticky-gecko")
	if !ok || tenant.Slug != "home-operations" {
		t.Fatalf("Installation(sticky-gecko) = %v, %v, %v", in, tenant, ok)
	}
	if in.App.PrivateKeyValue().Value() == "" || in.WebhookSecretValue().Value() != "whsec" || in.App.ClientIDValue() != "Iv1.xxxxxxxx" {
		t.Fatal("github app credentials not resolved")
	}
	br, _, _ := f.Installation("bot-ross")
	if br.App.ClientIDValue() != "Iv1.fromenv" {
		t.Fatalf("clientIdFrom not resolved: %q", br.App.ClientIDValue())
	}
	fj, _, ok := f.Installation("onedr0p-forgejo")
	if !ok || fj.TokenValue().Value() != "fj-token" || fj.WebhookSecretValue().Value() != "whsec" {
		t.Fatal("forgejo credentials not resolved")
	}
	if _, _, ok := f.Installation("nope"); ok {
		t.Fatal("unknown installation should not resolve")
	}
}

func TestHashAndInstallationLookup(t *testing.T) {
	f, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Hash()) != 64 {
		t.Fatalf("hash = %q", f.Hash())
	}
	ho, _ := f.Tenant("home-operations")
	if in := f.InstallationFor(ho, &Repository{Name: "home-operations/flate"}); in == nil || in.Name != "sticky-gecko" {
		t.Fatalf("InstallationFor = %v", in)
	}
	if f.InstallationFor(ho, &Repository{Name: "someone-else/repo"}) != nil || f.InstallationFor(ho, &Repository{Name: "noslash"}) != nil {
		t.Fatal("unknown owner must not resolve")
	}
}

func TestRetentionAndIgnore(t *testing.T) {
	f, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.DisabledIndexGrace() != 720*time.Hour {
		t.Fatalf("grace = %s", f.DisabledIndexGrace())
	}
	if (&File{}).DisabledIndexGrace() != DefaultDisabledIndexGrace {
		t.Fatal("unset grace should fall back to the default")
	}
	ho, _ := f.Tenant("home-operations")
	got := f.Settings(ho, "sticky-gecko", "home-operations/flate").Ignore
	if len(got) != len(DefaultIgnore)+1 || got[len(got)-1] != "**/testdata/**" {
		t.Fatalf("ignore = %v", got)
	}
	if n := len(f.Settings(ho, "sticky-gecko", "home-operations/other").Ignore); n != len(DefaultIgnore) {
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
const minimal = `
tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: forgejo
        account: acme
        token: { env: TEST_FORGEJO_TOKEN }
        webhookSecret: { env: TEST_WEBHOOK_SECRET }
`

// githubMinimal is the smallest github installation; clientFields is spliced
// into the app block.
func githubMinimal(clientFields string) string {
	return `
tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: github
        account: acme
        app: { ` + clientFields + `privateKey: { env: TEST_FORGEJO_TOKEN }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }
`
}

func TestProviders(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
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
			f, err := Parse([]byte("providers:\n" + tt.yaml + minimal))
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

func TestParseRejects(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("TEST_EMPTY", "")

	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("minimal fixture must parse: %v", err)
	}

	tests := []struct {
		name string
		yaml string
		want string // substring of the error
	}{
		{"empty file", "", "empty"},
		{"unknown top-level key", minimal + "tenant: []\n", "field tenant not found"},
		{"unknown nested key", strings.Replace(minimal, "account: acme", "account: acme\n        owner: acme", 1), "field owner not found"},
		{"no tenants", "tenants: []\n", "at least one tenant"},
		{"bad slug", strings.Replace(minimal, "slug: acme", "slug: Acme Corp", 1), "lowercase"},
		{"duplicate slug", minimal + strings.TrimPrefix(strings.Replace(minimal, "acme-bot", "acme-bot-2", 1), "\ntenants:\n"), "duplicates tenants[0]"},
		{"duplicate installation across tenants", minimal + strings.TrimPrefix(strings.Replace(minimal, "slug: acme", "slug: other", 1), "\ntenants:\n"), "names are hook paths"},
		{"no installations", "tenants:\n  - slug: acme\n    installations: []\n", "at least one installation"},
		{"missing account", strings.Replace(minimal, "        account: acme\n", "", 1), "account is required"},
		{"unknown forge", strings.Replace(minimal, "forge: forgejo", "forge: bitbucket", 1), "forge must be github or forgejo"},
		{"gitlab until it has a client", strings.Replace(minimal, "forge: forgejo", "forge: gitlab", 1), "forge gitlab is not supported yet"},
		{"github without app", strings.Replace(minimal, "forge: forgejo", "forge: github", 1), "needs an app"},
		{"github app with both client id forms", githubMinimal("clientId: x, clientIdFrom: { env: TEST_WEBHOOK_SECRET }, "), "exactly one of clientId or clientIdFrom"},
		{"github app with neither client id form", githubMinimal(""), "exactly one of clientId or clientIdFrom"},
		{"forgejo with app", strings.Replace(minimal, "token: { env: TEST_FORGEJO_TOKEN }", "app: { clientId: x, privateKey: { env: TEST_FORGEJO_TOKEN }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }", 1), "takes a token, not an app"},
		{"missing token", strings.Replace(minimal, "        token: { env: TEST_FORGEJO_TOKEN }\n", "", 1), "token is required"},
		{"unset env reference", strings.Replace(minimal, "TEST_FORGEJO_TOKEN", "TEST_DOES_NOT_EXIST", 1), "is not set"},
		{"empty env reference", strings.Replace(minimal, "TEST_FORGEJO_TOKEN", "TEST_EMPTY", 1), "token is required"},
		{"missing file reference", strings.Replace(minimal, "{ env: TEST_FORGEJO_TOKEN }", "{ file: /nonexistent/token }", 1), "no such file"},
		{"env and file both set", strings.Replace(minimal, "{ env: TEST_FORGEJO_TOKEN }", "{ env: TEST_FORGEJO_TOKEN, file: /x }", 1), "not both"},
		{"empty reference", strings.Replace(minimal, "{ env: TEST_FORGEJO_TOKEN }", "{}", 1), "token is required"},
		{"empty gitToken", minimal + "        gitToken: { env: TEST_EMPTY }\n", "gitToken resolved to an empty value"},
		{"unset gitToken", minimal + "        gitToken: { env: TEST_DOES_NOT_EXIST }\n", "gitToken"},
		{"github with gitToken", strings.TrimSuffix(githubMinimal("clientId: x, "), "\n") + "\n        gitToken: { env: TEST_FORGEJO_TOKEN }\n",
			"not token, gitToken or webhookSecret"},
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
		{"tenant model referencing undeclared provider", strings.Replace(minimal, "slug: acme", "slug: acme\n    models: { review: nope/gpt }", 1), "not declared under providers"},
		{"negative limit", "defaults:\n  limits:\n    reviewsPerDay: -1\n" + minimal, "must not be negative"},
		{"negative deadline", strings.Replace(minimal, "slug: acme", "slug: acme\n    runner: { activeDeadlineSeconds: -5 }", 1), "must not be negative"},
		{"negative retention", "retention:\n  disabledIndexGrace: -1h\n" + minimal, "retention.disabledIndexGrace"},
		{"negative settle default", "defaults:\n  settle: -1s\n" + minimal, "defaults.settle must not be negative"},
		{"unknown inline severity floor", "defaults:\n  review: { minSeverity: blocking }\n" + minimal, "defaults.review.minSeverity must be nit or important"},
		{"context without a description", "defaults:\n  review: { context: [{ path: db/schema.sql }] }\n" + minimal, "defaults.review.context[0]: description is required"},
		{"context outside the repository", "defaults:\n  review: { context: [{ path: ../x, description: x }] }\n" + minimal, "escapes the repository"},
		{"context with a bad glob", "defaults:\n  review: { context: [{ path: x, description: x, paths: ['['] }] }\n" + minimal, "paths[0] \"[\" is not a valid glob"},
		{"negative settle tenant", strings.Replace(minimal, "slug: acme", "slug: acme\n    settle: -1s", 1), "must not be negative"},
		{"negative settle repository", strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: [{ name: acme/x, settle: -1s }]", 1), "must not be negative"},
		{"indexing role removed", "defaults:\n  models:\n    indexing: p/m\n" + minimal, "field indexing not found"},
		{"bad ignore glob", strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: [{ name: acme/x, ignore: ['['] }]", 1), "not a valid glob"},
		{"filter syntax error", "defaults:\n  filter: 'pr.draft &&'\n" + minimal, "defaults.filter"},
		{"filter fails smoke test", "defaults:\n  filter: 'pr.labels[5].name == \"x\"'\n" + minimal, "smoke test"},
		{"repository filter error", strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: [{ name: acme/x, filter: 'pr.title' }]", 1), "repositories[0].filter"},
		{"repository without owner", strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: [{ name: x }]", 1), "owner/repo"},
		{"repository owner without installation", strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: [{ name: other/x }]", 1), "no installation in tenant"},
		{"duplicate repository", strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: [{ name: acme/x }, { name: acme/x }]", 1), "duplicates repositories[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
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
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")

	withRepo := func(repo string) string {
		return strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: ["+repo+"]", 1)
	}
	withDeadline := func(seconds int64) string {
		return strings.Replace(minimal, "slug: acme", fmt.Sprintf("slug: acme\n    runner: { activeDeadlineSeconds: %d }", seconds), 1)
	}

	tests := []struct {
		name string
		yaml string
		want string // substring of the error; empty means the config must be accepted
	}{
		{"runner deadline at the cap", withDeadline(int64(jobtimeout.MaxRunnerDeadline.Seconds())), ""},
		{"runner deadline past the cap", withDeadline(int64(jobtimeout.MaxRunnerDeadline.Seconds()) + 1), "runner.activeDeadlineSeconds must not exceed"},
		{"agent timeout at the cap", withRepo(fmt.Sprintf("{ name: acme/x, agent: { timeout: %ds } }", int64(jobtimeout.MaxAgentTimeout.Seconds()))), ""},
		{"agent timeout past the cap", withRepo(fmt.Sprintf("{ name: acme/x, agent: { timeout: %ds } }", int64(jobtimeout.MaxAgentTimeout.Seconds())+1)), "agent.timeout must not exceed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
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
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(minimal)

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
			t.Fatalf("%s: unexpected apply of %d tenants", why, len(f.Tenants))
		case <-time.After(150 * time.Millisecond):
		}
	}
	expectApply := func(slug string) {
		t.Helper()
		select {
		case f := <-applied:
			if f.Tenants[0].Slug != slug {
				t.Fatalf("applied slug %q, want %q", f.Tenants[0].Slug, slug)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no apply for %q", slug)
		}
	}

	expectNone("unchanged content on the first ticks")
	write(strings.Replace(minimal, "slug: acme", "slug: acme-two", 1))
	expectApply("acme-two")
	select {
	case <-rejected: // a tick that caught an earlier write half done
	default:
	}
	write("tenants: []\n")
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
	write(strings.Replace(minimal, "slug: acme", "slug: acme-two", 1))
	expectApply("acme-two")
	write(strings.Replace(minimal, "slug: acme", "slug: acme-three", 1))
	expectApply("acme-three")
}

// TestRepositoryInstallation checks that a repository entry binds to one
// installation when its owner's account has several, and that settings
// are looked up by installation and name, so the same owner/repo on two
// forges is two repositories.
func TestRepositoryInstallation(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	twoForges := func(repos string) string {
		return strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: ["+repos+"]", 1) + `      - name: acme-other
        forge: forgejo
        host: other.example.com
        account: acme
        token: { env: TEST_FORGEJO_TOKEN }
        webhookSecret: { env: TEST_WEBHOOK_SECRET }
`
	}
	refused := []struct {
		name, repos, want string
	}{
		{"an owner with several installations must name one", "{ name: acme/x }", "set installation to one of them"},
		{"the named installation must exist", "{ name: acme/x, installation: nope }", `installation "nope" is not an installation`},
		{"one installation may not list a repository twice", "{ name: acme/x, installation: acme-bot }, { name: acme/x, installation: acme-bot }",
			`duplicates repositories[0] of installation "acme-bot"`},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(twoForges(tt.repos))); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}

	t.Run("the same name under two installations is two repositories", func(t *testing.T) {
		f, err := Parse([]byte(twoForges("{ name: acme/x, installation: acme-other, mode: agentic }, { name: acme/x, installation: acme-bot }")))
		if err != nil {
			t.Fatal(err)
		}
		ten := &f.Tenants[0]
		if in := f.InstallationFor(ten, &ten.Repositories[0]); in == nil || in.Name != "acme-other" {
			t.Fatalf("InstallationFor = %v, want acme-other", in)
		}
		if got := f.Settings(ten, "acme-other", "acme/x").Mode; got != ReviewAgentic {
			t.Fatalf("acme-other mode = %q, want agentic", got)
		}
		if got := f.Settings(ten, "acme-bot", "acme/x").Mode; got != ReviewSingle {
			t.Fatalf("acme-bot mode = %q, want single", got)
		}
	})

	t.Run("an entry for one installation leaves the other forge's repository unlisted", func(t *testing.T) {
		f, err := Parse([]byte(twoForges("{ name: acme/x, installation: acme-other, enabled: false }")))
		if err != nil {
			t.Fatal(err)
		}
		ten := &f.Tenants[0]
		if f.Settings(ten, "acme-other", "acme/x").Enabled {
			t.Fatal("acme-other's acme/x should be disabled")
		}
		if !f.Settings(ten, "acme-bot", "acme/x").Enabled {
			t.Fatal("acme-bot's acme/x should keep the tenant's settings")
		}
	})
}

// TestPollingIndexingAndRunnerDefaults checks the tuning that lives in the
// file rather than the environment: its defaults, an explicit zero that
// turns polling off, and the tenant, defaults.runner, built-in order of a
// runner's deadline and resources.
func TestPollingIndexingAndRunnerDefaults(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")

	t.Run("defaults when unset", func(t *testing.T) {
		f, err := Parse([]byte(minimal))
		if err != nil {
			t.Fatal(err)
		}
		deadline, resources := f.RunnerFor(&f.Tenants[0])
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
		f, err := Parse([]byte(`
polling: { interval: 0s, lookback: 1h }
indexing: { onboardWindow: 8 }
defaults:
  runner: { activeDeadlineSeconds: 600, resources: { limits: { memory: 1Gi } } }
` + strings.Replace(minimal, "slug: acme", "slug: acme\n    runner: { activeDeadlineSeconds: 60 }", 1)))
		if err != nil {
			t.Fatal(err)
		}
		if f.PollInterval() != 0 || f.PollLookback() != time.Hour || f.OnboardWindow() != 8 {
			t.Fatalf("interval=%s lookback=%s window=%d", f.PollInterval(), f.PollLookback(), f.OnboardWindow())
		}
		deadline, resources := f.RunnerFor(&f.Tenants[0])
		if deadline != time.Minute || resources["limits"] == nil {
			t.Fatalf("tenant runner = %s %v; want the tenant's deadline over the default's resources", deadline, resources)
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
			if _, err := Parse([]byte(block + "\n" + minimal)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, want)
			}
		})
	}
}

// TestScopePrecedence checks ADR-0010 §2.4: every repository setting can be
// written at the defaults, a tenant and a repository entry, the narrowest
// one written wins even when it is empty or zero, and ignore globs add up.
func TestScopePrecedence(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
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
  review: { instructions: [ops/rules.md], templates: { summary: ops/summary.tmpl } }
  limits: { tokensPerMonth: 1000, reviewsPerDay: 5 }
`
	tenant := func(tenantKeys, repos string) string {
		return head + strings.Replace(minimal, "slug: acme", "slug: acme\n"+tenantKeys+"    repositories: ["+repos+"]", 1)
	}
	parse := func(t *testing.T, doc string) *File {
		t.Helper()
		f, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return f
	}

	t.Run("a tenant inherits what it leaves out", func(t *testing.T) {
		f := parse(t, tenant("", "{ name: acme/x }"))
		s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
		if s.Filter == nil || s.Settle != 2*time.Minute || s.Mode != ReviewAgentic || s.Agent.MaxSteps != 9 ||
			s.Incremental.MaxDeltaFiles != 3 || s.Models.Fallback != "p/small" || s.Limits.TokensPerMonth != 1000 ||
			!slices.Equal(s.Review.Instructions, []string{"ops/rules.md"}) || s.Review.Templates.Summary != "ops/summary.tmpl" {
			t.Fatalf("inherited settings = %+v", s)
		}
	})

	t.Run("an empty or zero value written at a narrower scope clears", func(t *testing.T) {
		f := parse(t, tenant(`    filter: ""
    settle: 0s
    models: { fallback: "" }
    limits: { tokensPerMonth: 0 }
`, `{ name: acme/x, review: { instructions: [], templates: { summary: "" } } }`))
		s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
		if s.Filter != nil || s.Settle != 0 || s.Models.Fallback != "" || s.Models.Review != "p/big" ||
			s.Limits.TokensPerMonth != 0 || s.Limits.ReviewsPerDay != 5 {
			t.Fatalf("cleared settings = %+v", s)
		}
		if len(s.Review.Instructions) != 0 || s.Review.Templates.Summary != "" {
			t.Fatalf("cleared review = %+v", s.Review)
		}
	})

	t.Run("the narrowest scope written wins, field by field", func(t *testing.T) {
		f := parse(t, tenant(`    mode: single
    agent: { maxSteps: 7 }
    review: { requireSuggestedFix: true }
    ignore: ["tenant/**"]
`, `{ name: acme/x, models: { review: p/small }, forks: true, agent: { maxTokens: 500 }, ignore: ["repo/**"] }`))
		s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
		if s.Mode != ReviewSingle || s.Agent.MaxSteps != 7 || s.Agent.MaxTokens != 500 || s.Models.Review != "p/small" || !s.Forks {
			t.Fatalf("settings = %+v", s)
		}
		if !s.Review.RequireSuggestedFix || !slices.Equal(s.Review.Instructions, []string{"ops/rules.md"}) {
			t.Fatalf("review = %+v, want the tenant's strictness over the defaults' instructions", s.Review)
		}
		want := append(append([]string(nil), DefaultIgnore...), "defaults/**", "tenant/**", "repo/**")
		if !slices.Equal(s.Ignore, want) {
			t.Fatalf("ignore = %v, want %v", s.Ignore, want)
		}
	})

	t.Run("an explicit concurrency must be positive", func(t *testing.T) {
		if _, err := Parse([]byte(tenant("    limits: { concurrency: 0 }\n", "{ name: acme/x }"))); err == nil ||
			!strings.Contains(err.Error(), "concurrency must be positive") {
			t.Fatalf("Parse = %v", err)
		}
	})
}

// TestReviewPresentation checks every finding goes inline unless a scope
// sets a severity floor or turns inline comments off.
func TestReviewPresentation(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	parse := func(t *testing.T, tenantKeys, repos string) *File {
		t.Helper()
		f, err := Parse([]byte(strings.Replace(minimal, "slug: acme", "slug: acme\n"+tenantKeys+"    repositories: ["+repos+"]", 1)))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return f
	}
	f := parse(t, "", "{ name: acme/x }")
	if s := f.Settings(&f.Tenants[0], "", ""); !s.Review.InlineComments || s.Review.MinSeverity != "" {
		t.Fatalf("review = %+v, want every finding inline", s.Review)
	}
	f = parse(t, "    review: { minSeverity: important, inlineComments: false }\n", "{ name: acme/x, review: { inlineComments: true } }")
	if s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x"); !s.Review.InlineComments || s.Review.MinSeverity != SeverityImportant {
		t.Fatalf("review = %+v, want the tenant's floor with the repository's inline comments", s.Review)
	}
}

// TestAllow checks the allow block resolves bound by bound like any other
// setting, and that load refuses a bound a repository could not choose
// and an operator value outside its own bounds.
func TestAllow(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
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
	doc := func(tenantKeys, repos string) string {
		return head + strings.Replace(minimal, "slug: acme", "slug: acme\n"+tenantKeys+"    repositories: ["+repos+"]", 1)
	}

	f, err := Parse([]byte(doc("    allow: { models: [p/big] }\n", "{ name: acme/x, allow: { settle: 5m, commands: [] } }")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
	a := s.Allow
	if !slices.Equal(a.Modes, []ReviewMode{ReviewSingle, ReviewAgentic}) || !slices.Equal(a.Models, []ModelRef{"p/big"}) ||
		a.Commands == nil || len(a.Commands) != 0 || *a.Agent.MaxSteps != 60 || *a.Agent.Timeout != 20*time.Minute ||
		a.Agent.MaxTokens != nil || *a.Settle != 5*time.Minute {
		t.Fatalf("allow = %+v", a)
	}
	if tenant := f.Settings(&f.Tenants[0], "", ""); *tenant.Allow.Settle != 30*time.Minute || !slices.Equal(tenant.Allow.Commands, []string{"rg", "fd"}) {
		t.Fatalf("tenant allow = %+v", tenant.Allow)
	}

	tests := []struct {
		name, yaml, want string
	}{
		{"an unknown mode", doc("    allow: { modes: [turbo] }\n", ""), "tenants[0].allow.modes[0] must be single or agentic"},
		{"a model of an undeclared provider", doc("    allow: { models: [q/big] }\n", ""), "allow.models[0] references provider \"q\""},
		{"a command path", doc("", "{ name: acme/x, allow: { commands: [/bin/sh] } }"), "allow.commands[0] \"/bin/sh\" must be a bare command name"},
		{"a bound that is not positive", doc("    allow: { agent: { maxTokens: 0 } }\n", ""), "allow.agent bounds must be positive"},
		{"a negative settle bound", doc("    allow: { settle: -1s }\n", ""), "allow.settle must not be negative"},
		{"the operator's mode outside its bounds", doc("    mode: agentic\n    allow: { modes: [single] }\n", ""), "mode agentic is outside allow.modes"},
		{"the operator's model outside its bounds", doc("    models: { fallback: p/tiny }\n", ""), "models.fallback \"p/tiny\" is outside allow.models"},
		{"a repository's command outside its bounds", doc("", "{ name: acme/x, agent: { commands: [curl] } }"), "agent.commands \"curl\" is outside allow.commands"},
		{"a built-in limit above its bound", doc("    allow: { agent: { maxSteps: 10 } }\n", ""), "agent.maxSteps is above allow.agent.maxSteps"},
		{"the operator's settle above its bound", doc("    settle: 1h\n", ""), "settle is above allow.settle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

// TestTools checks the tool catalog: what a run allowed some commands
// mounts, and the names, images, paths and commands load refuses.
func TestTools(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f, err := Parse([]byte(`
tools:
  - { name: helm, image: registry.example/helm:3, path: /usr/bin }
  - { name: flux-tools, image: registry.example/flux:2, commands: [flux, flate] }
` + minimal))
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
			if _, err := Parse([]byte("tools: [" + list + "]\n" + minimal)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, want)
			}
		})
	}
}

func TestRepositoryModeAgentReview(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	withRepo := func(repo string) string {
		return strings.Replace(minimal, "slug: acme", "slug: acme\n    repositories: ["+repo+"]", 1)
	}

	t.Run("defaults resolve when unset", func(t *testing.T) {
		f, err := Parse([]byte(withRepo("{ name: acme/x }")))
		if err != nil {
			t.Fatal(err)
		}
		for _, repo := range []string{"acme/x", "acme/unlisted"} {
			s := f.Settings(&f.Tenants[0], "acme-bot", repo)
			if s.Mode != ReviewSingle || !reflect.DeepEqual(s.Agent, DefaultAgent) || s.Incremental.MaxDeltaFiles != DefaultMaxDeltaFiles {
				t.Fatalf("%s: mode=%q agent=%+v incremental=%+v", repo, s.Mode, s.Agent, s.Incremental)
			}
			if s.Review.RequireSuggestedFix || len(s.Review.Instructions) != 0 || s.Review.Templates != (ReviewTemplates{}) {
				t.Fatalf("%s: review = %+v", repo, s.Review)
			}
		}
		if DefaultMaxDeltaFiles != 25 {
			t.Fatalf("DefaultMaxDeltaFiles = %d", DefaultMaxDeltaFiles)
		}
	})

	t.Run("repository values override the defaults", func(t *testing.T) {
		f, err := Parse([]byte(withRepo(`{ name: acme/x, mode: agentic,
      agent: { maxSteps: 12, maxToolOutputBytes: 4096, maxTokens: 250000, timeout: 3m, commands: [curl, rg], commandTimeout: 10s },
      incremental: { maxDeltaFiles: 5 },
      review: { instructions: [docs/rules.md], requireSuggestedFix: true,
        templates: { summary: .kritik/summary.md.tmpl, inline: .kritik/inline.md.tmpl } } }`)))
		if err != nil {
			t.Fatal(err)
		}
		s := f.Settings(&f.Tenants[0], "acme-bot", "acme/x")
		want := AgentSettings{MaxSteps: 12, MaxToolOutputBytes: 4096, MaxTokens: 250_000, Timeout: 3 * time.Minute,
			Commands: []string{"curl", "rg"}, CommandTimeout: 10 * time.Second}
		if s.Mode != ReviewAgentic || !reflect.DeepEqual(s.Agent, want) || s.Incremental.MaxDeltaFiles != 5 {
			t.Fatalf("mode=%q agent=%+v incremental=%+v", s.Mode, s.Agent, s.Incremental)
		}
		if !s.Review.RequireSuggestedFix || len(s.Review.Instructions) != 1 || s.Review.Instructions[0] != "docs/rules.md" ||
			s.Review.Templates.Summary != ".kritik/summary.md.tmpl" || s.Review.Templates.Inline != ".kritik/inline.md.tmpl" {
			t.Fatalf("review = %+v", s.Review)
		}
		if got := s.Review.Referenced(); strings.Join(got, ",") != "docs/rules.md,.kritik/summary.md.tmpl,.kritik/inline.md.tmpl" {
			t.Fatalf("referenced = %v", got)
		}
	})

	t.Run("a partial agent block keeps the other defaults", func(t *testing.T) {
		f, err := Parse([]byte(withRepo("{ name: acme/x, agent: { maxSteps: 7 } }")))
		if err != nil {
			t.Fatal(err)
		}
		want := DefaultAgent
		want.MaxSteps = 7
		if got := f.Settings(&f.Tenants[0], "acme-bot", "acme/x").Agent; !reflect.DeepEqual(got, want) {
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
		{"invalid mode", "{ name: acme/x, mode: loop }", "mode must be single or agentic"},
		{"zero max steps", "{ name: acme/x, agent: { maxSteps: 0 } }", "agent.maxSteps must be positive"},
		{"negative max steps", "{ name: acme/x, agent: { maxSteps: -1 } }", "agent.maxSteps must be positive"},
		{"zero tool output", "{ name: acme/x, agent: { maxToolOutputBytes: 0 } }", "agent.maxToolOutputBytes must be positive"},
		{"zero max tokens", "{ name: acme/x, agent: { maxTokens: 0 } }", "agent.maxTokens must be positive"},
		{"negative max tokens", "{ name: acme/x, agent: { maxTokens: -5 } }", "agent.maxTokens must be positive"},
		{"zero timeout", "{ name: acme/x, agent: { timeout: 0s } }", "agent.timeout must be positive"},
		{"zero delta files", "{ name: acme/x, incremental: { maxDeltaFiles: 0 } }", "incremental.maxDeltaFiles must be positive"},
		{"negative delta files", "{ name: acme/x, incremental: { maxDeltaFiles: -3 } }", "incremental.maxDeltaFiles must be positive"},
		{"unknown agent key", "{ name: acme/x, agent: { steps: 3 } }", "field steps not found"},
		{"zero command timeout", "{ name: acme/x, agent: { commandTimeout: 0s } }", "agent.commandTimeout must be at least 1s"},
		{"sub-second command timeout", "{ name: acme/x, agent: { commandTimeout: 500ms } }", "agent.commandTimeout must be at least 1s"},
		{"command path", "{ name: acme/x, agent: { commands: [/usr/bin/curl] } }", "must be a bare command name"},
		{"relative command path", "{ name: acme/x, agent: { commands: [./tool] } }", "must be a bare command name"},
		{"empty command", "{ name: acme/x, agent: { commands: [''] } }", "must be a bare command name"},
		{"duplicate command", "{ name: acme/x, agent: { commands: [rg, rg] } }", "is listed twice"},
		{"absolute instruction path", "{ name: acme/x, review: { instructions: [/etc/passwd] } }", "must be relative"},
		{"escaping template path", "{ name: acme/x, review: { templates: { summary: ../x.tmpl } } }", "escapes the repository"},
		{"empty instruction path", "{ name: acme/x, review: { instructions: [''] } }", "must not be empty"},
	}
	for _, tt := range rejects {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := Parse([]byte(withRepo(tt.repo)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
