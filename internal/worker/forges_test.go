package worker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
)

// appConnection is a connection of an App with clientID, its private
// key and webhook secret read from TEST_PRIVATE_KEY and TEST_WEBHOOK_SECRET.
func appConnection(t *testing.T, clientID string) *configfile.Connection {
	t.Helper()
	file, err := configfile.Parse([]byte(`
apps:
  - name: acme-bot
    accounts: [acme]
    clientId: ` + clientID + `
    privateKey: { env: TEST_PRIVATE_KEY }
    webhookSecret: { env: TEST_WEBHOOK_SECRET }
`))
	if err != nil {
		t.Fatal(err)
	}
	in, _ := file.Connection("acme-bot")
	return in
}

func TestBuildForgeRefusesAnotherForge(t *testing.T) {
	if _, err := BuildForge(t.Context(), &configfile.Connection{Name: "x", Forge: "gitlab"}, "acme/widgets"); err == nil {
		t.Fatal("BuildForge built a client for a forge kritik does not support")
	}
}

func TestForgeCacheRebuildsOnRotatedCredentials(t *testing.T) {
	t.Setenv("TEST_WEBHOOK_SECRET", "s")
	builds := 0
	cache := &ForgeCache{Build: func(context.Context, *configfile.Connection, string) (forge.Client, error) {
		builds++
		return nil, nil
	}}
	steps := []struct {
		name          string
		clientID, key string
		wantBuilds    int
	}{
		{"first use builds", "Iv1.a", "pem-a", 1},
		{"same credentials reuse", "Iv1.a", "pem-a", 1},
		{"rotated key rebuilds", "Iv1.a", "pem-b", 2},
		{"another client id rebuilds", "Iv1.b", "pem-b", 3},
		{"unchanged again reuses", "Iv1.b", "pem-b", 3},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv("TEST_PRIVATE_KEY", st.key)
			if _, err := cache.For(t.Context(), appConnection(t, st.clientID), "acme/widgets"); err != nil {
				t.Fatal(err)
			}
			if builds != st.wantBuilds {
				t.Fatalf("builds = %d, want %d", builds, st.wantBuilds)
			}
		})
	}
	if n := len(cache.clients); n != 1 {
		t.Fatalf("cache holds %d clients, want 1 per connection and owner", n)
	}
}

// TestForgeCacheBuildsPerOwner: a GitHub App has a connection, and a
// token, per account, so repositories of different owners get their own
// client and repositories of one owner share one.
func TestForgeCacheBuildsPerOwner(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "pem")
	t.Setenv("TEST_WEBHOOK_SECRET", "s")
	in := appConnection(t, "Iv1.a")
	var built []string
	cache := &ForgeCache{Build: func(_ context.Context, _ *configfile.Connection, repo string) (forge.Client, error) {
		built = append(built, repo)
		return nil, nil
	}}
	for _, repo := range []string{"acme/widgets", "acme/gadgets", "Other/tools", "other/more"} {
		if _, err := cache.For(t.Context(), in, repo); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"acme/widgets", "Other/tools"}; !slices.Equal(built, want) {
		t.Fatalf("built for %q, want %q", built, want)
	}
}

func TestCredentialFingerprint(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "pem-a")
	t.Setenv("TEST_WEBHOOK_SECRET", "wh")
	a := credentialFingerprint(appConnection(t, "Iv1.a"))
	if b := credentialFingerprint(appConnection(t, "Iv1.a")); a != b {
		t.Fatal("fingerprint is not stable")
	}
	if b := credentialFingerprint(appConnection(t, "Iv1.b")); a == b {
		t.Fatal("client id change kept the fingerprint")
	}
	t.Setenv("TEST_PRIVATE_KEY", "pem-b")
	if b := credentialFingerprint(appConnection(t, "Iv1.a")); a == b {
		t.Fatal("private key change kept the fingerprint")
	}
	if strings.Contains(a, "pem-a") || strings.Contains(a, "Iv1.a") {
		t.Fatal("fingerprint carries credential material")
	}
}
