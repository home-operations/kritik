package worker

import (
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
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
