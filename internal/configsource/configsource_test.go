package configsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/store"
)

const fileYAML = `
tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: forgejo
        account: acme
        token: { env: TEST_CS_TOKEN }
        webhookSecret: { env: TEST_CS_SECRET }
`

// fakeStore is the dashboard side of a Source: rows and a fingerprint the
// test sets, and a Listen that hands the test its handlers.
type fakeStore struct {
	mu       sync.Mutex
	rows     []configfile.DashboardTenant
	fp       string
	err      error
	handlers chan store.ListenHandlers
}

func newFakeStore() *fakeStore { return &fakeStore{handlers: make(chan store.ListenHandlers, 1)} }

func (f *fakeStore) set(fp string, rows ...configfile.DashboardTenant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.fp, f.err = rows, fp, nil
}

func (f *fakeStore) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStore) DashboardTenants(context.Context) ([]configfile.DashboardTenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.err
}

func (f *fakeStore) DashboardFingerprint(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fp, f.err
}

func (f *fakeStore) Listen(ctx context.Context, h store.ListenHandlers) {
	f.handlers <- h
	<-ctx.Done()
}

func testKeyring(t *testing.T) *sealbox.Keyring {
	t.Helper()
	k, err := sealbox.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// dashRow is a dashboard tenant slug with one forgejo installation inst,
// its token and webhook secret sealed with k.
func dashRow(t *testing.T, k *sealbox.Keyring, slug, inst string, rev int64) configfile.DashboardTenant {
	t.Helper()
	seal := func(v string) string {
		s, err := k.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	spec := `{"slug":"` + slug + `","installations":[{"name":"` + inst + `","forge":"forgejo","account":"` + slug + `",` +
		`"token":{"sealed":"` + seal("tok-"+slug) + `"},"webhookSecret":{"sealed":"` + seal("wh-"+slug) + `"}}]}`
	return configfile.DashboardTenant{Slug: slug, Spec: json.RawMessage(spec), Revision: rev}
}

func writeFile(t *testing.T, path, yaml string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configPath(t *testing.T) string {
	t.Helper()
	t.Setenv("TEST_CS_TOKEN", "tok")
	t.Setenv("TEST_CS_SECRET", "wh")
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, fileYAML)
	return path
}

// countingHandler counts records at error level.
type countingHandler struct{ errors atomic.Int32 }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.errors.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func hasInstallation(f *configfile.File, name string) bool {
	_, _, ok := f.Installation(name)
	return ok
}

func TestLoad(t *testing.T) {
	path := configPath(t)
	k := testKeyring(t)
	tests := []struct {
		name    string
		keyring *sealbox.Keyring
		rows    []configfile.DashboardTenant
		wantErr error
		want    []string
	}{
		{name: "file only", want: []string{"acme-bot"}},
		{name: "file only, no key needed", keyring: nil, want: []string{"acme-bot"}},
		{name: "file and dashboard", keyring: k, rows: []configfile.DashboardTenant{dashRow(t, k, "beta", "beta-bot", 1)},
			want: []string{"acme-bot", "beta-bot"}},
		{name: "dashboard rows without a key", rows: []configfile.DashboardTenant{dashRow(t, k, "beta", "beta-bot", 1)},
			wantErr: ErrNoDashboardKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			fs.set("fp", tt.rows...)
			s := &Source{Store: fs, Keyring: tt.keyring}
			f, err := s.Load(t.Context(), path)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Load = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if s.Current == nil || s.Current.Get() != f {
				t.Fatal("Load did not seed Current with the merged file")
			}
			for _, name := range tt.want {
				if !hasInstallation(f, name) {
					t.Fatalf("installation %s missing", name)
				}
			}
		})
	}

	t.Run("collision with the file", func(t *testing.T) {
		fs := newFakeStore()
		fs.set("fp", dashRow(t, k, "acme", "other-bot", 1))
		_, err := (&Source{Store: fs, Keyring: k}).Load(t.Context(), path)
		if _, ok := errors.AsType[*configfile.MergeError](err); !ok {
			t.Fatalf("Load = %v, want a *configfile.MergeError", err)
		}
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRun(t *testing.T) {
	path := configPath(t)
	k := testKeyring(t)
	fs := newFakeStore()
	logs := &countingHandler{}
	s := &Source{Store: fs, Keyring: k, Logger: slog.New(logs), Poll: time.Hour}
	if _, err := s.Load(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, path, 10*time.Millisecond) }()
	h := <-fs.handlers
	current := func() *configfile.File { return s.Current.Get() }
	// Sealing is randomised, so a row sealed twice is two different specs.
	beta1, beta2 := dashRow(t, k, "beta", "beta-bot", 1), dashRow(t, k, "beta", "beta-bot", 2)

	t.Run("a config notification merges the new row", func(t *testing.T) {
		fs.set("1", beta1)
		h.OnConfig("beta")
		waitFor(t, "beta-bot", func() bool { return hasInstallation(current(), "beta-bot") })
		in, _, _ := current().Installation("beta-bot")
		if in.TokenValue().Value() != "tok-beta" || in.WebhookSecretValue().Value() != "wh-beta" {
			t.Fatal("sealed credentials were not opened")
		}
	})

	t.Run("a repeat trigger with nothing new keeps the snapshot", func(t *testing.T) {
		before := current()
		changed := s.Current.Changed()
		h.OnReconnect()
		h.OnConfig("beta")
		time.Sleep(50 * time.Millisecond)
		select {
		case <-changed:
			t.Fatal("Current was replaced though nothing changed")
		default:
		}
		if current() != before {
			t.Fatal("snapshot changed")
		}
	})

	t.Run("a collision keeps the last good snapshot and logs once", func(t *testing.T) {
		before := current()
		fs.set("2", beta1, dashRow(t, k, "gamma", "acme-bot", 1))
		errsBefore := logs.errors.Load()
		h.OnConfig("gamma")
		waitFor(t, "LastError", func() bool { return s.LastError() != nil })
		h.OnConfig("gamma")
		h.OnReconnect()
		time.Sleep(50 * time.Millisecond)
		if current() != before {
			t.Fatal("a failed merge replaced the snapshot")
		}
		if _, ok := errors.AsType[*configfile.MergeError](s.LastError()); !ok {
			t.Fatalf("LastError = %v, want a *configfile.MergeError", s.LastError())
		}
		if n := logs.errors.Load() - errsBefore; n != 1 {
			t.Fatalf("logged %d errors for one distinct failure, want 1", n)
		}
	})

	t.Run("resolving the collision recovers", func(t *testing.T) {
		fs.set("3", beta2)
		h.OnReconnect()
		waitFor(t, "recovery", func() bool { return s.LastError() == nil })
		if !hasInstallation(current(), "acme-bot") || !hasInstallation(current(), "beta-bot") {
			t.Fatal("snapshot after recovery is missing an installation")
		}
	})

	t.Run("a store read failure keeps the snapshot", func(t *testing.T) {
		before := current()
		fs.fail(errors.New("connection refused"))
		h.OnConfig("beta")
		waitFor(t, "LastError", func() bool { return s.LastError() != nil })
		if current() != before || !strings.Contains(s.LastError().Error(), "connection refused") {
			t.Fatalf("snapshot replaced or error %v unexpected", s.LastError())
		}
		fs.set("3", beta2)
		h.OnConfig("beta")
		waitFor(t, "recovery", func() bool { return s.LastError() == nil })
		if current() != before {
			t.Fatal("recovering with unchanged inputs replaced the snapshot")
		}
	})

	t.Run("a file change is merged with the dashboard rows", func(t *testing.T) {
		writeFile(t, path, fileYAML+`
  - slug: zeta
    installations:
      - name: zeta-bot
        forge: forgejo
        account: zeta
        token: { env: TEST_CS_TOKEN }
        webhookSecret: { env: TEST_CS_SECRET }
`)
		waitFor(t, "zeta-bot", func() bool { return hasInstallation(current(), "zeta-bot") })
		if !hasInstallation(current(), "beta-bot") {
			t.Fatal("dashboard tenant lost on a file reload")
		}
	})

	t.Run("a deleted row drops the tenant", func(t *testing.T) {
		fs.set("4")
		h.OnConfig("beta")
		waitFor(t, "beta-bot gone", func() bool { return !hasInstallation(current(), "beta-bot") })
	})

	t.Run("rows without a key are refused at runtime too", func(t *testing.T) {
		keyless := &Source{Store: newFakeStore(), Current: configfile.NewCurrent(current()), Logger: slog.New(logs)}
		keyless.Store.(*fakeStore).set("5", dashRow(t, k, "beta", "beta-bot", 1))
		keyless.refresh(t.Context())
		if !errors.Is(keyless.LastError(), ErrNoDashboardKey) {
			t.Fatalf("LastError = %v, want ErrNoDashboardKey", keyless.LastError())
		}
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunPollsTheFingerprint(t *testing.T) {
	path := configPath(t)
	k := testKeyring(t)
	fs := newFakeStore()
	s := &Source{Store: fs, Keyring: k, Logger: slog.New(&countingHandler{}), Poll: 10 * time.Millisecond}
	if _, err := s.Load(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Run(t.Context(), path, time.Hour) }()
	<-fs.handlers
	fs.set("changed", dashRow(t, k, "beta", "beta-bot", 1))
	waitFor(t, "beta-bot via poll", func() bool { return hasInstallation(s.Current.Get(), "beta-bot") })
}
