package configsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/sealbox"
	"github.com/home-operations/kritik/internal/server"
	"github.com/home-operations/kritik/internal/store"
)

const fileYAML = `
accounts:
  - slug: acme
    connections:
      - name: acme-bot
        forge: github
        accounts: [acme]
        app: { clientId: Iv1.acme, privateKey: { env: TEST_CS_KEY }, webhookSecret: { env: TEST_CS_SECRET } }
`

// fakeStore is the dashboard side of a Source: rows and a fingerprint the
// test sets, and a Listen that hands the test its handlers.
type fakeStore struct {
	mu       sync.Mutex
	rows     []configfile.DashboardAccount
	fp       string
	err      error
	handlers chan store.ListenHandlers
}

func newFakeStore() *fakeStore { return &fakeStore{handlers: make(chan store.ListenHandlers, 1)} }

func (f *fakeStore) set(fp string, rows ...configfile.DashboardAccount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.fp, f.err = rows, fp, nil
}

func (f *fakeStore) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStore) DashboardAccounts(context.Context) ([]configfile.DashboardAccount, error) {
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

// dashRow is a dashboard account slug with one connection inst, its App's
// private key and webhook secret sealed with k.
func dashRow(t *testing.T, k *sealbox.Keyring, slug, inst string, rev int64) configfile.DashboardAccount {
	t.Helper()
	seal := func(v string) string {
		s, err := k.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	spec := `{"slug":"` + slug + `","connections":[{"name":"` + inst + `","forge":"github","accounts":["` + slug + `"],` +
		`"app":{"clientId":"Iv1.` + slug + `","privateKey":{"sealed":"` + seal("key-"+slug) + `"},"webhookSecret":{"sealed":"` + seal("wh-"+slug) + `"}}}]}`
	return configfile.DashboardAccount{Slug: slug, Spec: json.RawMessage(spec), Revision: rev}
}

func writeFile(t *testing.T, path, yaml string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configPath(t *testing.T) string {
	t.Helper()
	t.Setenv("TEST_CS_KEY", "tok")
	t.Setenv("TEST_CS_SECRET", "wh")
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, fileYAML)
	return path
}

// countingHandler counts records at error level, reloads and file accounts
// left out.
type countingHandler struct{ errors, reloads, skips atomic.Int32 }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.errors.Add(1)
	}
	switch r.Message {
	case "configuration reloaded":
		h.reloads.Add(1)
	case "configsource: file account left out of the running configuration":
		h.skips.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func dashRevision(f *configfile.File, slug string) int64 {
	for _, d := range f.Dashboard() {
		if d.Slug == slug {
			return d.Revision
		}
	}
	return 0
}

func hasConnection(f *configfile.File, name string) bool {
	_, _, ok := f.Connection(name)
	return ok
}

func TestLoad(t *testing.T) {
	path := configPath(t)
	k := testKeyring(t)
	tests := []struct {
		name    string
		keyring *sealbox.Keyring
		rows    []configfile.DashboardAccount
		wantErr error
		want    []string
	}{
		{name: "file only", want: []string{"acme-bot"}},
		{name: "file only, no key needed", keyring: nil, want: []string{"acme-bot"}},
		{name: "file and dashboard", keyring: k, rows: []configfile.DashboardAccount{dashRow(t, k, "beta", "beta-bot", 1)},
			want: []string{"acme-bot", "beta-bot"}},
		{name: "dashboard rows without a key", rows: []configfile.DashboardAccount{dashRow(t, k, "beta", "beta-bot", 1)},
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
				if !hasConnection(f, name) {
					t.Fatalf("connection %s missing", name)
				}
			}
		})
	}

	t.Run("a dashboard account holding the file's slug leaves the file account out", func(t *testing.T) {
		fs := newFakeStore()
		fs.set("fp", dashRow(t, k, "acme", "other-bot", 1))
		reg := prometheus.NewRegistry()
		f, err := (&Source{Store: fs, Keyring: k, Errors: server.NewConfigErrorGauge(reg)}).Load(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if acme, ok := f.Account("acme"); !ok || acme.Origin() != configfile.OriginDashboard || len(f.Skipped()) != 1 {
			t.Fatalf("account acme = %+v, skipped %v; want the dashboard's, with the file's left out", acme, f.Skipped())
		}
		if v := mergeGauge(t, reg); v != 1 {
			t.Fatalf("merge error gauge = %v, want 1", v)
		}
	})
}

// mergeGauge is the merge stage of the kritik_config_error gauge on reg.
func mergeGauge(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			if m.GetLabel()[0].GetValue() == "merge" {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatal("no merge series")
	return 0
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
	reg := prometheus.NewRegistry()
	s := &Source{Store: fs, Keyring: k, Logger: slog.New(logs), Poll: time.Hour, Errors: server.NewConfigErrorGauge(reg)}
	if _, err := s.Load(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, path, 10*time.Millisecond) }()
	h := <-fs.handlers
	current := func() *configfile.File { return s.Current.Get() }
	// Sealing is randomised, so a row sealed twice is two different specs.
	beta1, beta2, beta3 := dashRow(t, k, "beta", "beta-bot", 1), dashRow(t, k, "beta", "beta-bot", 2), dashRow(t, k, "beta", "beta-bot", 3)

	t.Run("a config notification merges the new row", func(t *testing.T) {
		fs.set("1", beta1)
		h.OnConfig("beta")
		waitFor(t, "beta-bot", func() bool { return hasConnection(current(), "beta-bot") })
		in, _, _ := current().Connection("beta-bot")
		if in.App.PrivateKeyValue().Value() != "key-beta" || in.WebhookSecretValue().Value() != "wh-beta" {
			t.Fatal("sealed credentials were not opened")
		}
	})

	// Triggers are handled one at a time in order, so once a later change is
	// live every trigger sent before it has been handled too.
	t.Run("a repeat trigger with nothing new keeps the snapshot", func(t *testing.T) {
		reloads := logs.reloads.Load()
		h.OnReconnect()
		h.OnConfig("beta")
		fs.set("2", beta2)
		h.OnConfig("beta")
		waitFor(t, "beta at revision 2", func() bool { return dashRevision(current(), "beta") == 2 })
		if n := logs.reloads.Load() - reloads; n != 1 {
			t.Fatalf("Current was set %d times for one change, want 1", n)
		}
	})

	t.Run("a collision keeps the last good snapshot and logs once", func(t *testing.T) {
		before := current()
		fs.set("3", beta2, dashRow(t, k, "gamma", "beta-bot", 1))
		errsBefore := logs.errors.Load()
		h.OnConfig("gamma")
		waitFor(t, "LastError", func() bool { return s.LastError() != nil })
		if current() != before {
			t.Fatal("a failed merge replaced the snapshot")
		}
		if _, ok := errors.AsType[*configfile.MergeError](s.LastError()); !ok {
			t.Fatalf("LastError = %v, want a *configfile.MergeError", s.LastError())
		}
		if v := mergeGauge(t, reg); v != 1 {
			t.Fatalf("merge error gauge = %v while failing, want 1", v)
		}
		h.OnConfig("gamma")
		h.OnReconnect()
		fs.set("4", beta3)
		h.OnReconnect()
		waitFor(t, "recovery", func() bool { return s.LastError() == nil && dashRevision(current(), "beta") == 3 })
		if n := logs.errors.Load() - errsBefore; n != 1 {
			t.Fatalf("logged %d errors for one distinct failure, want 1", n)
		}
		if v := mergeGauge(t, reg); v != 0 {
			t.Fatalf("merge error gauge = %v after recovery, want 0", v)
		}
		if _, ok := current().Account("gamma"); ok || !hasConnection(current(), "acme-bot") {
			t.Fatal("snapshot after recovery is wrong")
		}
	})

	t.Run("a dashboard account holding a file connection name leaves that account out, warning once", func(t *testing.T) {
		skips := logs.skips.Load()
		fs.set("5", beta3, dashRow(t, k, "gamma", "acme-bot", 1))
		h.OnConfig("gamma")
		waitFor(t, "gamma", func() bool { _, ok := current().Account("gamma"); return ok })
		if _, ok := current().Account("acme"); ok || s.LastError() != nil {
			t.Fatalf("the file account still runs, or the merge failed: %v", s.LastError())
		}
		want := []configfile.SkippedAccount{{Slug: "acme", Reason: `dashboard account "gamma" already holds connection name "acme-bot"`}}
		if got := current().Skipped(); !slices.Equal(got, want) {
			t.Fatalf("Skipped = %v, want %v", got, want)
		}
		waitFor(t, "merge gauge raised", func() bool { return mergeGauge(t, reg) == 1 })
		fs.set("6", beta3, dashRow(t, k, "gamma", "acme-bot", 2))
		h.OnConfig("gamma")
		waitFor(t, "gamma at revision 2", func() bool { return dashRevision(current(), "gamma") == 2 })
		fs.set("4", beta3)
		h.OnConfig("gamma")
		waitFor(t, "acme back", func() bool { _, ok := current().Account("acme"); return ok })
		waitFor(t, "merge gauge cleared", func() bool { return mergeGauge(t, reg) == 0 })
		if n := logs.skips.Load() - skips; n != 1 {
			t.Fatalf("warned %d times for one clash, want 1", n)
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
		fs.set("4", beta3)
		h.OnConfig("beta")
		waitFor(t, "recovery", func() bool { return s.LastError() == nil })
		if current() != before {
			t.Fatal("recovering with unchanged inputs replaced the snapshot")
		}
	})

	t.Run("a file change is merged with the dashboard rows", func(t *testing.T) {
		writeFile(t, path, fileYAML+`
  - slug: zeta
    connections:
      - name: zeta-bot
        forge: github
        accounts: [zeta]
        app: { clientId: Iv1.zeta, privateKey: { env: TEST_CS_KEY }, webhookSecret: { env: TEST_CS_SECRET } }
`)
		waitFor(t, "zeta-bot", func() bool { return hasConnection(current(), "zeta-bot") })
		if !hasConnection(current(), "beta-bot") {
			t.Fatal("dashboard account lost on a file reload")
		}
	})

	t.Run("a deleted row drops the account", func(t *testing.T) {
		fs.set("4")
		h.OnConfig("beta")
		waitFor(t, "beta-bot gone", func() bool { return !hasConnection(current(), "beta-bot") })
	})

	t.Run("a file that does not parse raises the gauge until one that does replaces it", func(t *testing.T) {
		good, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "accounts: []\n")
		waitFor(t, "merge gauge", func() bool { return mergeGauge(t, reg) == 1 })
		// A dashboard change still merges onto the last good file, and does
		// not clear the gauge the bad file raised.
		fs.set("6", dashRow(t, k, "beta", "beta-bot", 4))
		h.OnConfig("beta")
		waitFor(t, "beta-bot", func() bool { return hasConnection(current(), "beta-bot") })
		if v := mergeGauge(t, reg); v != 1 {
			t.Fatalf("merge error gauge = %v while the file is bad, want 1", v)
		}
		// Reverting to the very content last applied clears it too.
		writeFile(t, path, string(good))
		waitFor(t, "merge gauge cleared", func() bool { return mergeGauge(t, reg) == 0 })
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
	waitFor(t, "beta-bot via poll", func() bool { return hasConnection(s.Current.Get(), "beta-bot") })
}
