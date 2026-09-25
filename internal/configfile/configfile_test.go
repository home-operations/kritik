package configfile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			konflate string
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
				name: "listed repo adds konflate", tenant: ho, repo: "home-operations/flate",
				enabled: true, review: "openrouter/openai/gpt-6-sol", conc: 3, perDay: 200, konflate: "https://konflate.example.org",
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
				s := f.Settings(tt.tenant, tt.repo)
				if s.Enabled != tt.enabled || s.Models.Review != tt.review || s.Forks != tt.forks ||
					s.Limits.Concurrency != tt.conc || s.Limits.ReviewsPerDay != tt.perDay || s.Konflate != tt.konflate ||
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
		if got := g.Settings(&g.Tenants[0], "x/y").Limits.Concurrency; got != DefaultConcurrency {
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
	if in := f.InstallationFor(ho, "home-operations/flate"); in == nil || in.Name != "sticky-gecko" {
		t.Fatalf("InstallationFor = %v", in)
	}
	if f.InstallationFor(ho, "someone-else/repo") != nil || f.InstallationFor(ho, "noslash") != nil {
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
	for kk, vv := range pr {
		out[kk] = vv
	}
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
		{"unknown forge", strings.Replace(minimal, "forge: forgejo", "forge: bitbucket", 1), "forge must be"},
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, path, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), func(f *File) { applied <- f })

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
	write("tenants: []\n")
	expectNone("an invalid file must not be applied")
	write(strings.Replace(minimal, "slug: acme", "slug: acme-three", 1))
	expectApply("acme-three")
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
			s := f.Settings(&f.Tenants[0], repo)
			if s.Mode != ReviewSingle || s.Agent != DefaultAgent || s.Incremental.MaxDeltaFiles != DefaultMaxDeltaFiles {
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
      agent: { maxSteps: 12, maxToolOutputBytes: 4096, timeout: 3m },
      incremental: { maxDeltaFiles: 5 },
      review: { instructions: [docs/rules.md], requireSuggestedFix: true,
        templates: { summary: .kritik/summary.md.j2, inline: .kritik/inline.md.j2 } } }`)))
		if err != nil {
			t.Fatal(err)
		}
		s := f.Settings(&f.Tenants[0], "acme/x")
		want := AgentSettings{MaxSteps: 12, MaxToolOutputBytes: 4096, Timeout: 3 * time.Minute}
		if s.Mode != ReviewAgentic || s.Agent != want || s.Incremental.MaxDeltaFiles != 5 {
			t.Fatalf("mode=%q agent=%+v incremental=%+v", s.Mode, s.Agent, s.Incremental)
		}
		if !s.Review.RequireSuggestedFix || len(s.Review.Instructions) != 1 || s.Review.Instructions[0] != "docs/rules.md" ||
			s.Review.Templates.Summary != ".kritik/summary.md.j2" || s.Review.Templates.Inline != ".kritik/inline.md.j2" {
			t.Fatalf("review = %+v", s.Review)
		}
		if got := s.Review.Referenced(); strings.Join(got, ",") != "docs/rules.md,.kritik/summary.md.j2,.kritik/inline.md.j2" {
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
		if got := f.Settings(&f.Tenants[0], "acme/x").Agent; got != want {
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
		{"zero timeout", "{ name: acme/x, agent: { timeout: 0s } }", "agent.timeout must be positive"},
		{"zero delta files", "{ name: acme/x, incremental: { maxDeltaFiles: 0 } }", "incremental.maxDeltaFiles must be positive"},
		{"negative delta files", "{ name: acme/x, incremental: { maxDeltaFiles: -3 } }", "incremental.maxDeltaFiles must be positive"},
		{"unknown agent key", "{ name: acme/x, agent: { steps: 3 } }", "field steps not found"},
		{"absolute instruction path", "{ name: acme/x, review: { instructions: [/etc/passwd] } }", "must be relative"},
		{"escaping template path", "{ name: acme/x, review: { templates: { summary: ../x.j2 } } }", "escapes the repository"},
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
