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
connections:
  - name: acme-bot
    forge: github
    accounts: [acme]
    app: { clientId: Iv1.acme, privateKey: { env: TEST_CS_KEY }, webhookSecret: { env: TEST_CS_SECRET } }
`

// fakeStore is the dashboard side of a Source: a spec and a fingerprint the
// test sets, and a Listen that hands the test its handlers.
type fakeStore struct {
	mu       sync.Mutex
	spec     configfile.InstanceSpec
	fp       string
	err      error
	handlers chan store.ListenHandlers
}

func newFakeStore() *fakeStore { return &fakeStore{handlers: make(chan store.ListenHandlers, 1)} }

func (f *fakeStore) set(fp string, spec configfile.InstanceSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec, f.fp, f.err = spec, fp, nil
}

func (f *fakeStore) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStore) InstanceSpec(context.Context) (configfile.InstanceSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spec, f.err
}

func (f *fakeStore) InstanceSpecFingerprint(context.Context) (string, error) {
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

// specOf is a spec at rev holding one connection per "name:account" in
// conns, each App's private key and webhook secret sealed with k.
func specOf(t *testing.T, k *sealbox.Keyring, rev int64, conns ...string) configfile.InstanceSpec {
	t.Helper()
	seal := func(v string) string {
		s, err := k.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	list := make([]string, len(conns))
	for i, c := range conns {
		name, account, _ := strings.Cut(c, ":")
		list[i] = `{"name":"` + name + `","forge":"github","accounts":["` + account + `"],` +
			`"app":{"clientId":"Iv1.` + name + `","privateKey":{"sealed":"` + seal("key-"+name) + `"},"webhookSecret":{"sealed":"` + seal("wh-"+name) + `"}}}`
	}
	return configfile.InstanceSpec{Spec: json.RawMessage(`{"connections":[` + strings.Join(list, ",") + `]}`), Revision: rev}
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

// countingHandler counts records at error level, reloads and file
// connections left out.
type countingHandler struct{ errors, reloads, skips atomic.Int32 }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.errors.Add(1)
	}
	switch r.Message {
	case "configuration reloaded":
		h.reloads.Add(1)
	case "configsource: file connection left out of the running configuration":
		h.skips.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func hasConnection(f *configfile.File, name string) bool {
	_, ok := f.Connection(name)
	return ok
}

func TestLoad(t *testing.T) {
	path := configPath(t)
	k := testKeyring(t)
	tests := []struct {
		name    string
		keyring *sealbox.Keyring
		spec    configfile.InstanceSpec
		wantErr error
		want    []string
	}{
		{name: "file only", want: []string{"acme-bot"}},
		{name: "file only, no key needed", keyring: nil, want: []string{"acme-bot"}},
		{name: "file and spec", keyring: k, spec: specOf(t, k, 1, "beta-bot:beta"), want: []string{"acme-bot", "beta-bot"}},
		{name: "a spec without a key", spec: specOf(t, k, 1, "beta-bot:beta"), wantErr: ErrNoDashboardKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			fs.set("fp", tt.spec)
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

	t.Run("no file at all", func(t *testing.T) {
		f, err := (&Source{Store: newFakeStore()}).Load(t.Context(), "")
		if err != nil || len(f.Connections) != 0 {
			t.Fatalf("Load = %+v, %v", f, err)
		}
	})

	t.Run("a spec connection holding the file's name leaves the file's out", func(t *testing.T) {
		fs := newFakeStore()
		fs.set("fp", specOf(t, k, 1, "acme-bot:other"))
		reg := prometheus.NewRegistry()
		f, err := (&Source{Store: fs, Keyring: k, Errors: server.NewConfigErrorGauge(reg)}).Load(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if in, ok := f.Connection("acme-bot"); !ok || in.Origin() != configfile.OriginDashboard || len(f.Skipped()) != 1 {
			t.Fatalf("connection acme-bot = %+v, skipped %v; want the spec's, with the file's left out", in, f.Skipped())
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
	done := make(chan struct{})
	go func() {
		s.Run(ctx, path, 10*time.Millisecond)
		close(done)
	}()
	h := <-fs.handlers
	current := func() *configfile.File { return s.Current.Get() }
	revision := func() int64 { return current().Spec().Revision }
	// Sealing is randomised, so a spec sealed twice is two different specs.
	beta1, beta2, beta3 := specOf(t, k, 1, "beta-bot:beta"), specOf(t, k, 2, "beta-bot:beta"), specOf(t, k, 3, "beta-bot:beta")

	t.Run("a config notification merges the new spec", func(t *testing.T) {
		fs.set("1", beta1)
		h.OnConfig("1")
		waitFor(t, "beta-bot", func() bool { return hasConnection(current(), "beta-bot") })
		in, _ := current().Connection("beta-bot")
		if in.App.PrivateKeyValue().Value() != "key-beta-bot" || in.WebhookSecretValue().Value() != "wh-beta-bot" {
			t.Fatal("sealed credentials were not opened")
		}
	})

	// Triggers are handled one at a time in order, so once a later change is
	// live every trigger sent before it has been handled too.
	t.Run("a repeat trigger with nothing new keeps the snapshot", func(t *testing.T) {
		reloads := logs.reloads.Load()
		h.OnReconnect()
		h.OnConfig("1")
		fs.set("2", beta2)
		h.OnConfig("2")
		waitFor(t, "revision 2", func() bool { return revision() == 2 })
		if n := logs.reloads.Load() - reloads; n != 1 {
			t.Fatalf("Current was set %d times for one change, want 1", n)
		}
	})

	t.Run("a spec that does not merge keeps the last good snapshot and logs once", func(t *testing.T) {
		before := current()
		fs.set("3", specOf(t, k, 3, "beta-bot:beta", "beta-bot:gamma"))
		errsBefore := logs.errors.Load()
		h.OnConfig("3")
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
		h.OnConfig("3")
		h.OnReconnect()
		fs.set("4", beta3)
		h.OnReconnect()
		waitFor(t, "recovery", func() bool { return s.LastError() == nil && revision() == 3 })
		if n := logs.errors.Load() - errsBefore; n != 1 {
			t.Fatalf("logged %d errors for one distinct failure, want 1", n)
		}
		if v := mergeGauge(t, reg); v != 0 {
			t.Fatalf("merge error gauge = %v after recovery, want 0", v)
		}
		if !hasConnection(current(), "acme-bot") {
			t.Fatal("snapshot after recovery is wrong")
		}
	})

	t.Run("a spec connection holding a file connection's name leaves the file's out, warning once", func(t *testing.T) {
		skips := logs.skips.Load()
		fs.set("5", specOf(t, k, 4, "beta-bot:beta", "acme-bot:gamma"))
		h.OnConfig("4")
		waitFor(t, "revision 4", func() bool { return revision() == 4 })
		if in, _ := current().Connection("acme-bot"); in.Origin() != configfile.OriginDashboard || s.LastError() != nil {
			t.Fatalf("the file connection still runs, or the merge failed: %v", s.LastError())
		}
		want := []configfile.SkippedConnection{{Name: "acme-bot", Reason: `the dashboard's connection "acme-bot" already holds the name`}}
		if got := current().Skipped(); !slices.Equal(got, want) {
			t.Fatalf("Skipped = %v, want %v", got, want)
		}
		waitFor(t, "merge gauge raised", func() bool { return mergeGauge(t, reg) == 1 })
		fs.set("6", specOf(t, k, 5, "beta-bot:beta", "acme-bot:gamma"))
		h.OnConfig("5")
		waitFor(t, "revision 5", func() bool { return revision() == 5 })
		fs.set("4", beta3)
		h.OnConfig("3")
		waitFor(t, "acme-bot back", func() bool {
			in, ok := current().Connection("acme-bot")
			return ok && in.Origin() == configfile.OriginFile
		})
		waitFor(t, "merge gauge cleared", func() bool { return mergeGauge(t, reg) == 0 })
		if n := logs.skips.Load() - skips; n != 1 {
			t.Fatalf("warned %d times for one clash, want 1", n)
		}
	})

	t.Run("a store read failure keeps the snapshot", func(t *testing.T) {
		before := current()
		fs.fail(errors.New("connection refused"))
		h.OnConfig("3")
		waitFor(t, "LastError", func() bool { return s.LastError() != nil })
		if current() != before || !strings.Contains(s.LastError().Error(), "connection refused") {
			t.Fatalf("snapshot replaced or error %v unexpected", s.LastError())
		}
		fs.set("4", beta3)
		h.OnConfig("3")
		waitFor(t, "recovery", func() bool { return s.LastError() == nil })
		if current() != before {
			t.Fatal("recovering with unchanged inputs replaced the snapshot")
		}
	})

	t.Run("a file change is merged with the spec", func(t *testing.T) {
		writeFile(t, path, fileYAML+`  - name: zeta-bot
    forge: github
    accounts: [zeta]
    app: { clientId: Iv1.zeta, privateKey: { env: TEST_CS_KEY }, webhookSecret: { env: TEST_CS_SECRET } }
`)
		waitFor(t, "zeta-bot", func() bool { return hasConnection(current(), "zeta-bot") })
		if !hasConnection(current(), "beta-bot") {
			t.Fatal("spec connection lost on a file reload")
		}
	})

	t.Run("a spec emptied drops its connections", func(t *testing.T) {
		fs.set("7", configfile.InstanceSpec{Spec: json.RawMessage(`{}`), Revision: 6})
		h.OnConfig("6")
		waitFor(t, "beta-bot gone", func() bool { return !hasConnection(current(), "beta-bot") })
	})

	t.Run("a file that does not parse raises the gauge until one that does replaces it", func(t *testing.T) {
		good, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "accounts: []\n")
		waitFor(t, "merge gauge", func() bool { return mergeGauge(t, reg) == 1 })
		// A spec change still merges onto the last good file, and does not
		// clear the gauge the bad file raised.
		fs.set("8", specOf(t, k, 7, "beta-bot:beta"))
		h.OnConfig("7")
		waitFor(t, "beta-bot", func() bool { return hasConnection(current(), "beta-bot") })
		if v := mergeGauge(t, reg); v != 1 {
			t.Fatalf("merge error gauge = %v while the file is bad, want 1", v)
		}
		// Reverting to the very content last applied clears it too.
		writeFile(t, path, string(good))
		waitFor(t, "merge gauge cleared", func() bool { return mergeGauge(t, reg) == 0 })
	})

	t.Run("a spec without a key is refused at runtime too", func(t *testing.T) {
		keyless := &Source{Store: newFakeStore(), Current: configfile.NewCurrent(current()), Logger: slog.New(logs)}
		keyless.Store.(*fakeStore).set("5", specOf(t, k, 1, "beta-bot:beta"))
		keyless.refresh(t.Context())
		if !errors.Is(keyless.LastError(), ErrNoDashboardKey) {
			t.Fatalf("LastError = %v, want ErrNoDashboardKey", keyless.LastError())
		}
	})

	cancel()
	<-done
}

func TestRunPollsTheFingerprint(t *testing.T) {
	path := configPath(t)
	k := testKeyring(t)
	fs := newFakeStore()
	s := &Source{Store: fs, Keyring: k, Logger: slog.New(&countingHandler{}), Poll: 10 * time.Millisecond}
	if _, err := s.Load(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	go s.Run(t.Context(), path, time.Hour)
	<-fs.handlers
	fs.set("changed", specOf(t, k, 1, "beta-bot:beta"))
	waitFor(t, "beta-bot via poll", func() bool { return hasConnection(s.Current.Get(), "beta-bot") })
}
