package configfile

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeOpener opens a sealed value "sealed:<plain>" to <plain>.
type fakeOpener struct{}

func (fakeOpener) Open(sealed string) ([]byte, error) {
	plain, ok := strings.CutPrefix(sealed, "sealed:")
	if !ok {
		return nil, errors.New("not sealed by this key")
	}
	return []byte(plain + "\n"), nil
}

// dashSpec is a valid dashboard tenant spec for slug with one forgejo
// installation named inst.
func dashSpec(slug, inst string) string {
	return `{"slug":"` + slug + `","installations":[{"name":"` + inst + `","forge":"forgejo","account":"` + slug + `",` +
		`"token":{"sealed":"sealed:tok-` + slug + `"},"webhookSecret":{"sealed":"sealed:wh-` + slug + `"}}]}`
}

func dash(slug, spec string, rev int64) DashboardTenant {
	return DashboardTenant{Slug: slug, Spec: json.RawMessage(spec), Revision: rev}
}

func parseMinimal(t *testing.T) *File {
	t.Helper()
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	f, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParseRejectsSealed(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"installation token", strings.Replace(minimal, "{ env: TEST_FORGEJO_TOKEN }", "{ sealed: abc }", 1),
			"tenants[0].installations[0].token: sealed values are only valid in dashboard-managed tenants"},
		{"provider key", "providers:\n  p:\n    type: openai\n    apiKey: { sealed: abc }\n" + minimal,
			"providers.p.apiKey: sealed values are only valid in dashboard-managed tenants"},
		{"egress credential", "egress:\n  allowHosts: [api.example.com]\n  credentials:\n    api.example.com: { sealed: abc }\n" + minimal,
			"egress.credentials.api.example.com: sealed values are only valid"},
		{"sealed and env", strings.Replace(minimal, "{ env: TEST_FORGEJO_TOKEN }", "{ env: TEST_FORGEJO_TOKEN, sealed: abc }", 1),
			"set exactly one of env, file or sealed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not mention %q", err, tt.want)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	file := parseMinimal(t)
	m, err := Merge(file, []DashboardTenant{dash("beta", dashSpec("beta", "beta-bot"), 3)}, fakeOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	t.Run("dashboard installation is looked up like a file one", func(t *testing.T) {
		in, ten, ok := m.Installation("beta-bot")
		if !ok {
			t.Fatal("beta-bot not found")
		}
		if ten.Slug != "beta" || ten.Origin() != OriginDashboard {
			t.Fatalf("tenant = %q origin %q", ten.Slug, ten.Origin())
		}
		if in.WebhookSecretValue().Value() != "wh-beta" || in.TokenValue().Value() != "tok-beta" {
			t.Fatalf("secrets = %q %q", in.WebhookSecretValue().Value(), in.TokenValue().Value())
		}
		if m.InstallationFor(ten, "beta/repo") == nil {
			t.Fatal("InstallationFor found nothing")
		}
		if got := m.Settings(ten, "beta/repo"); !got.Enabled || got.Limits.Concurrency != DefaultConcurrency {
			t.Fatalf("settings = %+v", got)
		}
	})

	t.Run("file tenants keep origin file", func(t *testing.T) {
		ten, ok := m.Tenant("acme")
		if !ok || ten.Origin() != OriginFile {
			t.Fatalf("acme origin = %v", ten)
		}
		if _, _, ok := m.Installation("acme-bot"); !ok {
			t.Fatal("acme-bot lost")
		}
	})

	t.Run("file is not mutated", func(t *testing.T) {
		if len(file.Tenants) != 1 || len(file.Dashboard()) != 0 {
			t.Fatalf("file tenants = %d dashboard = %d", len(file.Tenants), len(file.Dashboard()))
		}
		if _, _, ok := file.Installation("beta-bot"); ok {
			t.Fatal("dashboard installation leaked into the file")
		}
	})

	t.Run("dashboard inputs are remembered", func(t *testing.T) {
		d := m.Dashboard()
		if len(d) != 1 || d[0].Slug != "beta" || d[0].Revision != 3 {
			t.Fatalf("Dashboard() = %+v", d)
		}
	})

	t.Run("merging a merged file replaces its dashboard tenants", func(t *testing.T) {
		again, err := Merge(m, []DashboardTenant{dash("gamma", dashSpec("gamma", "gamma-bot"), 1)}, fakeOpener{})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if _, ok := again.Tenant("beta"); ok || len(again.Tenants) != 2 {
			t.Fatalf("tenants = %d, beta kept = %v", len(again.Tenants), ok)
		}
	})
}

func TestMergeHash(t *testing.T) {
	file := parseMinimal(t)
	a, b := dash("alpha", dashSpec("alpha", "alpha-bot"), 1), dash("beta", dashSpec("beta", "beta-bot"), 2)
	hash := func(d ...DashboardTenant) string {
		t.Helper()
		m, err := Merge(file, d, fakeOpener{})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		return m.Hash()
	}
	if got := hash(); got != file.Hash() {
		t.Fatalf("no dashboard tenants: hash %s, want the file's %s", got, file.Hash())
	}
	if hash(a, b) != hash(b, a) {
		t.Fatal("hash depends on input order")
	}
	if hash(a, b) == file.Hash() {
		t.Fatal("dashboard tenants do not change the hash")
	}
	b2 := b
	b2.Revision = 3
	if hash(a, b) == hash(a, b2) {
		t.Fatal("a revision bump does not change the hash")
	}
}

func TestMergeRejects(t *testing.T) {
	file := parseMinimal(t)
	tests := []struct {
		name string
		d    DashboardTenant
		open Opener
		want string
	}{
		{"slug collides with the file", dash("acme", dashSpec("acme", "other-bot"), 1), fakeOpener{}, "duplicates tenants[0]"},
		{"installation collides with the file", dash("beta", dashSpec("beta", "acme-bot"), 1), fakeOpener{}, "names are hook paths"},
		{"slug mismatch", dash("beta", dashSpec("gamma", "gamma-bot"), 1), fakeOpener{}, "does not match"},
		{"env ref", dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), `{"sealed":"sealed:tok-beta"}`, `{"env":"TEST_FORGEJO_TOKEN"}`, 1), 1),
			fakeOpener{}, "installations[0].token: dashboard-managed tenants take sealed values"},
		{"file ref", dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), `{"sealed":"sealed:tok-beta"}`, `{"file":"/etc/passwd"}`, 1), 1),
			fakeOpener{}, "dashboard-managed tenants take sealed values"},
		{"nil opener", dash("beta", dashSpec("beta", "beta-bot"), 1), nil, "no key to open sealed values"},
		{"open fails", dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), "sealed:tok-beta", "garbage", 1), 1),
			fakeOpener{}, "not sealed by this key"},
		{"bad filter", dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), `"installations"`, `"filter":"pr.draft &&","installations"`, 1), 1),
			fakeOpener{}, "configfile: dashboard[beta].filter: "},
		{"trailing document", dash("beta", dashSpec("beta", "beta-bot")+"\n---\n{}", 1), fakeOpener{}, "one document"},
		{"trailing content", dash("beta", dashSpec("beta", "beta-bot")+" x", 1), fakeOpener{}, "tenant spec"},
		{"host outside the default allowlist", dash("beta", withHost(dashSpec("beta", "beta-bot"), "evil.example.com"), 1), fakeOpener{},
			`dashboard[beta].installations[0].host: "evil.example.com" is not an allowed dashboard forge host`},
		{"unknown key", dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), `"installations"`, `"nope":1,"installations"`, 1), 1),
			fakeOpener{}, "field nope not found"},
		{"empty spec", dash("beta", "", 1), fakeOpener{}, "empty"},
		{"undeclared provider", dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), `"installations"`, `"models":{"review":"x/y"},"installations"`, 1), 1),
			fakeOpener{}, "not declared under providers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Merge(file, []DashboardTenant{tt.d}, tt.open)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not mention %q", err, tt.want)
			}
			me, ok := errors.AsType[*MergeError](err)
			if !ok || me.Slug != tt.d.Slug {
				t.Fatalf("error %v is not a *MergeError for %q", err, tt.d.Slug)
			}
			if len(file.Tenants) != 1 {
				t.Fatal("file mutated")
			}
		})
	}
}

func TestValidateDashboard(t *testing.T) {
	file := parseMinimal(t)
	m, err := Merge(file, []DashboardTenant{dash("beta", dashSpec("beta", "beta-bot"), 1)}, fakeOpener{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		d    DashboardTenant
		ok   bool
	}{
		{"replace existing with a new installation name", dash("beta", dashSpec("beta", "beta-bot-2"), 2), true},
		{"add another", dash("gamma", dashSpec("gamma", "gamma-bot"), 1), true},
		{"add one colliding with an existing dashboard tenant", dash("gamma", dashSpec("gamma", "beta-bot"), 1), false},
		{"add one colliding with the file", dash("acme", dashSpec("acme", "x-bot"), 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDashboard(m, tt.d, fakeOpener{})
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

func TestOriginValid(t *testing.T) {
	for o, want := range map[Origin]bool{OriginFile: true, OriginDashboard: true, "": false, "git": false} {
		if o.Valid() != want {
			t.Errorf("%q.Valid() = %v", o, !want)
		}
	}
	var zero Tenant
	if zero.Origin() != OriginFile {
		t.Fatal("zero tenant origin is not file")
	}
}

func withHost(spec, host string) string {
	return strings.Replace(spec, `"forge":"forgejo"`, `"forge":"forgejo","host":"`+host+`"`, 1)
}

func TestDecodeTenantDuration(t *testing.T) {
	ten, err := DecodeTenant(dash("beta", strings.Replace(dashSpec("beta", "beta-bot"), `"installations"`, `"settle":"5m","installations"`, 1), 1))
	if err != nil {
		t.Fatalf("DecodeTenant: %v", err)
	}
	if ten.Settle != 5*time.Minute {
		t.Fatalf("settle = %s, want 5m", ten.Settle)
	}
}

func TestDashboardForgeHosts(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	withFileHost := strings.Replace(minimal, "forge: forgejo", "forge: forgejo\n        host: https://Git.Example.com:3000", 1)
	explicit := "web:\n  dashboardForgeHosts: [Code.Example.org]\n" + withFileHost
	tests := []struct {
		name string
		file string
		host string // "" for a github installation without a host
		ok   bool
	}{
		{"default allows github.com", withFileHost, "", true},
		{"default allows a file installation's host, case-insensitively", withFileHost, "git.EXAMPLE.com", true},
		{"default rejects another host", withFileHost, "code.example.org", false},
		{"explicit list allows its host", explicit, "https://code.example.org", true},
		{"explicit list replaces the default", explicit, "git.example.com", false},
		{"explicit list without github.com rejects github", explicit, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := Parse([]byte(tt.file))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			spec := withHost(dashSpec("beta", "beta-bot"), tt.host)
			if tt.host == "" {
				spec = `{"slug":"beta","installations":[{"name":"beta-bot","forge":"github","account":"beta",` +
					`"app":{"clientId":"x","privateKey":{"sealed":"sealed:k"},"webhookSecret":{"sealed":"sealed:w"}}}]}`
			}
			_, err = Merge(file, []DashboardTenant{dash("beta", spec, 1)}, fakeOpener{})
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok %v", err, tt.ok)
			}
			if err != nil {
				if me, ok := errors.AsType[*MergeError](err); !ok || me.Slug != "beta" || !strings.Contains(err.Error(), "installations[0].host") {
					t.Fatalf("error %v is not a *MergeError naming the host path", err)
				}
			}
		})
	}
}
