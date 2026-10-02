package worker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

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

func TestAppsRefuseAnotherForge(t *testing.T) {
	apps := &Apps{}
	other := &configfile.Connection{Name: "x", Forge: "gitlab"}
	if _, err := apps.Build(t.Context(), other, "acme/widgets"); err == nil {
		t.Fatal("Build built a client for a forge kritika does not support")
	}
	if _, err := apps.Reach(t.Context(), other); err == nil {
		t.Fatal("Reach listed a forge kritika does not support")
	}
}

// TestAppsBuildOnePerConnection: a connection's clients and listings
// share one App, and with it its installation tokens; another connection
// gets its own.
func TestAppsBuildOnePerConnection(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	t.Setenv("TEST_WEBHOOK_SECRET", "s")
	acme, globex := appConnection(t, "Iv1.acme"), appConnection(t, "Iv1.globex")
	globex.Name = "globex-bot"
	apps := &Apps{}
	first, err := apps.app(acme)
	if err != nil {
		t.Fatal(err)
	}
	again, err := apps.app(acme)
	if err != nil {
		t.Fatal(err)
	}
	other, err := apps.app(globex)
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == other || len(apps.apps) != 2 {
		t.Fatalf("apps = %d, want one per connection, the same on every use", len(apps.apps))
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

// TestForgeCacheBuildsOutsideItsLock: a build that hangs on the forge
// holds up neither another connection's client nor a caller whose context
// ends meanwhile, and the callers of its own key share the one build.
func TestForgeCacheBuildsOutsideItsLock(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "pem")
	t.Setenv("TEST_WEBHOOK_SECRET", "s")
	slow, fast := appConnection(t, "Iv1.slow"), appConnection(t, "Iv1.fast")
	fast.Name = "fast-bot"
	release := make(chan struct{})
	var mu sync.Mutex
	builds := map[string]int{}
	cache := &ForgeCache{Build: func(_ context.Context, in *configfile.Connection, _ string) (forge.Client, error) {
		mu.Lock()
		builds[in.Name]++
		mu.Unlock()
		if in == slow {
			<-release
		}
		return nil, nil
	}}
	// Two callers of the slow connection share one build.
	started := make(chan struct{}, 2)
	for range 2 {
		go func() {
			started <- struct{}{}
			_, _ = cache.For(context.Background(), slow, "acme/widgets")
		}()
	}
	<-started
	<-started
	done := make(chan error, 1)
	go func() {
		_, err := cache.For(context.Background(), fast, "acme/widgets")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("another connection's client waited on the slow build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := cache.For(ctx, slow, "acme/gadgets"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a caller whose context ended got %v, want its deadline", err)
	}
	close(release)
	if _, err := cache.For(context.Background(), slow, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if builds[slow.Name] != 1 || builds[fast.Name] != 1 {
		t.Fatalf("builds = %v, want one per connection", builds)
	}
}
