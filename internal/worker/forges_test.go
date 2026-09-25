package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/forge/forgejo"
)

// forgejoConfigYAML is the smallest valid Forgejo installation: a token, no
// app block (Forgejo, unlike GitHub, authenticates with a bot token).
const forgejoConfigYAML = `
tenants:
  - slug: acme
    installations:
      - name: acme-forgejo
        forge: forgejo
        host: https://forge.example.com
        account: acme
        token: { env: TEST_FORGEJO_BUILD_TOKEN }
        webhookSecret: { env: TEST_FORGEJO_BUILD_SECRET }
`

func TestBuildForgeReturnsForgejoClient(t *testing.T) {
	t.Setenv("TEST_FORGEJO_BUILD_TOKEN", "tok")
	t.Setenv("TEST_FORGEJO_BUILD_SECRET", "s")
	file, err := configfile.Parse([]byte(forgejoConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	in, _, ok := file.Installation("acme-forgejo")
	if !ok {
		t.Fatal("installation not found")
	}

	// externalID and repo are GitHub-only concerns (installation discovery);
	// Forgejo's client construction needs neither.
	client, err := BuildForge(t.Context(), in, 0, "acme/widgets")
	if err != nil {
		t.Fatalf("BuildForge: %v", err)
	}
	if _, ok := client.(*forgejo.Client); !ok {
		t.Fatalf("BuildForge returned %T, want *forgejo.Client", client)
	}
}

func TestBuildForgeGitToken(t *testing.T) {
	t.Setenv("TEST_FORGEJO_BUILD_TOKEN", "api-token")
	t.Setenv("TEST_FORGEJO_BUILD_SECRET", "s")
	t.Setenv("TEST_FORGEJO_FETCH_TOKEN", "fetch-token")
	tests := []struct {
		name, extra, want string
	}{
		{name: "the API token when no gitToken is set", want: "api-token"},
		{name: "the gitToken when set", extra: "        gitToken: { env: TEST_FORGEJO_FETCH_TOKEN }\n", want: "fetch-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := configfile.Parse([]byte(forgejoConfigYAML + tt.extra))
			if err != nil {
				t.Fatal(err)
			}
			in, _, _ := file.Installation("acme-forgejo")
			client, err := BuildForge(t.Context(), in, 0, "acme/widgets")
			if err != nil {
				t.Fatal(err)
			}
			if got, err := client.GitToken(t.Context()); err != nil || got != tt.want {
				t.Fatalf("GitToken = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestForgeCacheRebuildsOnRotatedCredentials(t *testing.T) {
	load := func(t *testing.T, token, gitToken string) *configfile.Installation {
		t.Helper()
		t.Setenv("TEST_FORGEJO_BUILD_TOKEN", token)
		t.Setenv("TEST_FORGEJO_BUILD_SECRET", "s")
		yaml := forgejoConfigYAML
		if gitToken != "" {
			t.Setenv("TEST_FORGEJO_BUILD_GIT", gitToken)
			yaml += "        gitToken: { env: TEST_FORGEJO_BUILD_GIT }\n"
		}
		file, err := configfile.Parse([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		in, _, _ := file.Installation("acme-forgejo")
		return in
	}
	builds := 0
	cache := &ForgeCache{Build: func(context.Context, *configfile.Installation, int64, string) (forge.Client, error) {
		builds++
		return nil, nil
	}}
	steps := []struct {
		name       string
		token, git string
		wantBuilds int
	}{
		{"first use builds", "tok", "", 1},
		{"same credentials reuse", "tok", "", 1},
		{"rotated token rebuilds", "tok2", "", 2},
		{"added git token rebuilds", "tok2", "git", 3},
		{"rotated git token rebuilds", "tok2", "git2", 4},
		{"unchanged again reuses", "tok2", "git2", 4},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			if _, err := cache.For(t.Context(), load(t, st.token, st.git), 0, "acme/widgets"); err != nil {
				t.Fatal(err)
			}
			if builds != st.wantBuilds {
				t.Fatalf("builds = %d, want %d", builds, st.wantBuilds)
			}
		})
	}
	if n := len(cache.clients); n != 1 {
		t.Fatalf("cache holds %d clients, want 1 per installation", n)
	}
}

func TestCredentialFingerprint(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "pem-a")
	t.Setenv("TEST_WEBHOOK_SECRET", "wh")
	app := func(t *testing.T, clientID string) *configfile.Installation {
		t.Helper()
		file, err := configfile.Parse([]byte(`
tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: github
        account: acme
        app: { clientId: ` + clientID + `, privateKey: { env: TEST_FORGEJO_TOKEN }, webhookSecret: { env: TEST_WEBHOOK_SECRET } }
`))
		if err != nil {
			t.Fatal(err)
		}
		in, _, _ := file.Installation("acme-bot")
		return in
	}
	a := credentialFingerprint(app(t, "Iv1.a"))
	if b := credentialFingerprint(app(t, "Iv1.a")); a != b {
		t.Fatal("fingerprint is not stable")
	}
	if b := credentialFingerprint(app(t, "Iv1.b")); a == b {
		t.Fatal("client id change kept the fingerprint")
	}
	t.Setenv("TEST_FORGEJO_TOKEN", "pem-b")
	if b := credentialFingerprint(app(t, "Iv1.a")); a == b {
		t.Fatal("private key change kept the fingerprint")
	}
	if strings.Contains(a, "pem-a") || strings.Contains(a, "Iv1.a") {
		t.Fatal("fingerprint carries credential material")
	}
}
