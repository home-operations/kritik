package configfile

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// specConnection is a dashboard connection named name serving accounts,
// its secrets sealed for testOpener.
func specConnection(name string, accounts ...string) string {
	list, _ := json.Marshal(accounts)
	return `{"name":"` + name + `","forge":"github","accounts":` + string(list) +
		`,"app":{"clientId":"Iv1.` + name + `","privateKey":{"sealed":"test:key-` + name + `"},"webhookSecret":{"sealed":"test:wh-` + name + `"}}}`
}

func spec(raw string, rev int64) InstanceSpec {
	return InstanceSpec{Spec: json.RawMessage(raw), Revision: rev}
}

func parseMinimal(t *testing.T) *File {
	t.Helper()
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	file, _ := split(t, minimal)
	f, err := Parse(file)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParseFileLayer(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Run("no file at all", func(t *testing.T) {
		for _, raw := range [][]byte{nil, []byte("  \n")} {
			f, err := Parse(raw)
			if err != nil || len(f.Connections) != 0 || f.Hash() == "" {
				t.Fatalf("Parse(%q) = %+v, %v", raw, f, err)
			}
		}
	})
	const file = `connections:
  - { name: acme-bot, forge: github, accounts: [acme], app: { clientId: x, privateKey: { env: TEST_PRIVATE_KEY }, webhookSecret: { env: TEST_WEBHOOK_SECRET } } }
`
	if _, err := Parse([]byte(file)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, tt := range []struct{ name, yaml, want string }{
		{"a spec key", "providers: {}\n" + file, "field providers not found"},
		{"a sealed connection secret", strings.Replace(file, "{ env: TEST_PRIVATE_KEY }", "{ sealed: abc }", 1),
			"connections[0].app.privateKey: sealed values are only valid in the dashboard's configuration"},
		{"sealed and env", strings.Replace(file, "{ env: TEST_PRIVATE_KEY }", "{ env: TEST_PRIVATE_KEY, sealed: abc }", 1),
			"set exactly one of env, file or sealed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v does not mention %q", err, tt.want)
			}
		})
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
	file, _ := split(t, minimal)

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

func TestMerge(t *testing.T) {
	file := parseMinimal(t)
	m, err := Merge(file, spec(`{
		"providers": {"p": {"type": "openai", "apiKey": {"sealed": "test:sk"}}},
		"defaults": {"models": {"review": "p/big"}, "settle": "2m"},
		"connections": [`+specConnection("dash-bot", "org-2", "Org-3")+`],
		"accounts": [
			{"forge": "github", "name": "ORG-2", "limits": {"reviewsPerDay": 5}, "repositories": [{"name": "repo-1", "enabled": false}]},
			{"forge": "github", "name": "gone", "forks": true}
		]
	}`, 4), testOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if m.Providers["p"].APIKeyValue().Value() != "sk" || m.Spec().Revision != 4 {
		t.Fatalf("providers = %+v, spec = %+v", m.Providers, m.Spec())
	}
	names := make([]string, 0, len(m.Connections))
	for _, in := range m.Connections {
		names = append(names, in.Name+":"+string(in.Origin()))
	}
	if !slices.Equal(names, []string{"acme-bot:file", "dash-bot:dashboard"}) {
		t.Fatalf("connections = %v", names)
	}
	accounts := make([]string, 0, len(m.Accounts))
	for _, a := range m.Accounts {
		accounts = append(accounts, a.Slug())
	}
	if !slices.Equal(accounts, []string{"github/acme", "github/ORG-2", "github/Org-3"}) {
		t.Fatalf("accounts = %v", accounts)
	}
	org2, ok := m.Account(ForgeGitHub, "org-2")
	if !ok || org2.Limits.ReviewsPerDay == nil || m.Settings(org2, "org-2/repo-1").Enabled || m.Settings(org2, "").Settle != 2*time.Minute {
		t.Fatalf("org-2 = %+v", org2)
	}
	if in := m.ConnectionFor(org2); in == nil || in.Name != "dash-bot" {
		t.Fatalf("ConnectionFor(org-2) = %v", in)
	}
	if u := m.Unserved(); len(u) != 1 || u[0].Name != "gone" {
		t.Fatalf("unserved = %+v", u)
	}
	if _, ok := m.Account(ForgeGitHub, "gone"); ok {
		t.Fatal("an unserved account runs")
	}
	again, err := Merge(m, InstanceSpec{}, testOpener{})
	if err != nil || len(again.Connections) != 1 || len(again.Accounts) != 1 || again.Hash() != file.Hash() {
		t.Fatalf("merging a merged file does not replace its spec: %+v, %v", again, err)
	}
}

func TestMergeHash(t *testing.T) {
	file := parseMinimal(t)
	hash := func(s InstanceSpec) string {
		t.Helper()
		m, err := Merge(file, s, testOpener{})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		return m.Hash()
	}
	a := spec(`{"defaults":{"forks":true}}`, 1)
	if hash(InstanceSpec{}) != file.Hash() {
		t.Fatal("no spec: the hash is not the file's")
	}
	if hash(a) == file.Hash() {
		t.Fatal("a spec does not change the hash")
	}
	if hash(a) == hash(spec(`{"defaults":{"forks":true}}`, 2)) {
		t.Fatal("a revision bump does not change the hash")
	}
	if hash(a) == hash(spec(`{"defaults":{"forks":false}}`, 1)) {
		t.Fatal("a different spec at the same revision does not change the hash")
	}
}

func TestMergeRejects(t *testing.T) {
	file := parseMinimal(t)
	for _, tt := range []struct{ name, spec, want string }{
		{"an unknown key", `{"tenants":[]}`, "field tenants not found"},
		{"an auth key", `{"auth":{}}`, "field auth not found"},
		{"an env reference", `{"providers":{"p":{"type":"openai","apiKey":{"env":"HOME"}}}}`,
			"providers.p.apiKey: the dashboard's configuration takes sealed values"},
		{"a value that does not open", `{"providers":{"p":{"type":"openai","apiKey":{"sealed":"garbage"}}}}`, "open sealed value"},
		{"a broken account entry", `{"accounts":[{"forge":"github","name":"acme","models":{"review":"nope/x"}}]}`,
			`accounts[0].models.review references provider "nope"`},
		{"a duplicate spec connection", `{"connections":[` + specConnection("a", "x") + `,` + specConnection("a", "y") + `]}`,
			"names are hook paths"},
		{"an account two spec connections serve", `{"connections":[` + specConnection("a", "x") + `,` + specConnection("b", "X") + `]}`,
			"an account is served by one connection"},
		{"two documents", `{} {}`, "one document"},
		{"an embedder with no endpoint", embeddingSpec(`"baseUrl":"embed.example"`), "embedding.baseUrl"},
		{"an embedder with no model", embeddingSpec(`"model":""`), "embedding.model is required"},
		{"an embedder too wide for the index", embeddingSpec(`"dims":4096`), "embedding.dims must be between 1 and 4000"},
		{"an embedder with a negative bound", embeddingSpec(`"maxBatch":-1`), "must not be negative"},
		{"an embedder key that does not open", `{"embedding":{"baseUrl":"https://e.example/v1","apiKey":{"sealed":"garbage"},"model":"m","dims":8}}`,
			"embedding.apiKey"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Merge(file, spec(tt.spec, 1), testOpener{})
			if _, ok := errors.AsType[*MergeError](err); !ok || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Merge = %v, want a *MergeError containing %q", err, tt.want)
			}
		})
	}
}

// embeddingSpec is a spec whose embedder is valid but for override, a
// field that replaces the one of its name.
func embeddingSpec(override string) string {
	fields := map[string]string{
		"baseUrl": `"baseUrl":"https://embed.example/v1"`, "apiKey": `"apiKey":{"sealed":"test:ek"}`,
		"model": `"model":"voyage-code-3"`, "dims": `"dims":1024`,
	}
	key, _, _ := strings.Cut(strings.Trim(override, `"`), `"`)
	fields[key] = override
	return `{"embedding":{` + strings.Join(slices.Sorted(maps.Values(fields)), ",") + `}}`
}

func TestMergeEmbedding(t *testing.T) {
	file := parseMinimal(t)
	if m, err := Merge(file, spec(`{}`, 1), testOpener{}); err != nil || m.Embedding != nil {
		t.Fatalf("no embedder: %+v, %v", m.Embedding, err)
	}
	m, err := Merge(file, spec(embeddingSpec(`"maxBatch":8`), 2), testOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	e := m.Embedding
	if e == nil || e.Model != "voyage-code-3" || e.Dims != 1024 || e.APIKeyValue().Value() != "ek" {
		t.Fatalf("embedding = %+v", e)
	}
	if batch, chars, item := e.Bounds(); batch != 8 || chars != DefaultEmbedMaxBatchChars || item != DefaultEmbedMaxItemChars {
		t.Fatalf("Bounds = %d, %d, %d", batch, chars, item)
	}
}

func TestMergeAccountProviders(t *testing.T) {
	file := parseMinimal(t)
	m, err := Merge(file, spec(`{"accounts":[{"forge":"github","name":"acme",
		"providers":{"own":{"type":"anthropic","apiKey":{"sealed":"test:sk-acme"}}},"models":{"review":"own/big"}}]}`, 1), testOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if p, ok := m.Provider(&m.Accounts[0], "own"); !ok || p.APIKeyValue().Value() != "sk-acme" {
		t.Fatalf("Provider(acme, own) = %+v, %v", p, ok)
	}
}

// TestMergeSkipsFileConnectionsTheSpecHolds: a file connection whose name
// or account a spec connection holds is left out, not a failed merge.
func TestMergeSkipsFileConnectionsTheSpecHolds(t *testing.T) {
	file := parseMinimal(t)
	for _, tt := range []struct{ name, conn, reason string }{
		{"by name", specConnection("acme-bot", "org-9"), `the dashboard's connection "acme-bot" already holds the name`},
		{"by account", specConnection("dash-bot", "ACME"), `the dashboard's connection "dash-bot" already serves account "acme"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Merge(file, spec(`{"connections":[`+tt.conn+`]}`, 1), testOpener{})
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if sk := m.Skipped(); len(sk) != 1 || sk[0].Name != "acme-bot" || sk[0].Reason != tt.reason {
				t.Fatalf("skipped = %+v", sk)
			}
			if len(m.Connections) != 1 || m.Connections[0].Origin() != OriginDashboard {
				t.Fatalf("connections = %+v", m.Connections)
			}
			if fc := m.FileConnections(); len(fc) != 1 || fc[0].Name != "acme-bot" {
				t.Fatalf("FileConnections = %+v", fc)
			}
		})
	}
}

// TestValidateSpec: a write may not claim a file connection's name or
// account, unless the stored spec already held it.
func TestValidateSpec(t *testing.T) {
	file := parseMinimal(t)
	byName := spec(`{"connections":[`+specConnection("acme-bot", "org-9")+`]}`, 2)
	byAccount := spec(`{"connections":[`+specConnection("dash-bot", "acme")+`]}`, 2)
	for _, tt := range []struct {
		name         string
		stored, next InstanceSpec
		want         string
	}{
		{"a file connection's name", InstanceSpec{}, byName, `connections[0].name "acme-bot" is declared in the configuration file`},
		{"a file connection's account", InstanceSpec{}, byAccount, `connections[0].accounts[0] "acme" is served by connection "acme-bot"`},
		{"a name the stored spec held", byName, byName, ""},
		{"an account the stored spec held", byAccount, byAccount, ""},
		{"a spec that does not merge", InstanceSpec{}, spec(`{"polling":{"interval":"-1m"}}`, 1), "must not be negative"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSpec(file, tt.stored, tt.next, testOpener{})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ValidateSpec = %v", err)
				}
				return
			}
			if _, ok := errors.AsType[*MergeError](err); !ok || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateSpec = %v, want a *MergeError containing %q", err, tt.want)
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
	var zero Connection
	if zero.Origin() != OriginFile {
		t.Fatal("zero connection origin is not file")
	}
}

func TestDecodeSpecDuration(t *testing.T) {
	s, err := DecodeSpec(json.RawMessage(`{"defaults":{"settle":"5m"}}`))
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if s.Defaults.Settle == nil || *s.Defaults.Settle != 5*time.Minute {
		t.Fatalf("settle = %v, want 5m", s.Defaults.Settle)
	}
}
