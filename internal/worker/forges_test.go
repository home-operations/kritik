package worker

import (
	"context"
	"slices"
	"testing"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
)

// appConnection is a connection of an App with clientID, its private
// key and webhook secret read from TEST_PRIVATE_KEY and TEST_WEBHOOK_SECRET.
func appConnection(t *testing.T, clientID string) *configfile.Connection {
	t.Helper()
	file, err := configfile.Parse([]byte(`
apps:
  acme-bot:
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
	if _, err := BuildForge(t.Context(), &configfile.Connection{Name: "x", Forge: "gitlab"}, "acme/widgets", nil); err == nil {
		t.Fatal("BuildForge built a client for a forge kritika does not support")
	}
}

func TestForgeCacheBuildsOnce(t *testing.T) {
	t.Setenv("TEST_WEBHOOK_SECRET", "s")
	t.Setenv("TEST_PRIVATE_KEY", "pem")
	builds := 0
	cache := &ForgeCache{Build: func(context.Context, *configfile.Connection, string) (forge.Client, error) {
		builds++
		return nil, nil
	}}
	in := appConnection(t, "Iv1.a")
	for range 3 {
		if _, err := cache.For(t.Context(), in, "acme/widgets"); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || len(cache.clients) != 1 {
		t.Fatalf("builds = %d, clients = %d; want one per connection and owner", builds, len(cache.clients))
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
