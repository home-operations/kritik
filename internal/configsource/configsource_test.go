package configsource

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/home-operations/kritik/internal/server"
)

const signIn = "auth: { admin: { password: { env: TEST_ADMIN_PASSWORD } } }\n"

func write(t *testing.T, path, doc string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSourceKeepsTheLastGoodConfiguration: a reload that does not load, or
// that leaves the dashboard no sign-in, is refused and reported while the
// last good configuration keeps running, and content that loads again
// clears the report.
func TestSourceKeepsTheLastGoodConfiguration(t *testing.T) {
	t.Setenv("TEST_ADMIN_PASSWORD", "pw")
	path := filepath.Join(t.TempDir(), "kritik.yaml")
	write(t, path, signIn+"egress: { allowHosts: [first.example] }\n")
	reg := prometheus.NewRegistry()
	src := &Source{RequireSignIn: true, Errors: server.NewConfigErrorGauge(reg)}
	if _, err := src.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	failing := func() float64 {
		t.Helper()
		v, err := testutil.GatherAndCount(reg, "kritik_config_error")
		if err != nil || v != 2 {
			t.Fatalf("gauge series = %d, %v", v, err)
		}
		mfs, _ := reg.Gather()
		for _, m := range mfs[0].GetMetric() {
			if m.GetLabel()[0].GetValue() == string(server.ConfigErrorLoad) {
				return m.GetGauge().GetValue()
			}
		}
		t.Fatal("no load stage")
		return 0
	}
	go src.Run(t.Context(), path, 10*time.Millisecond)
	// The watcher takes the file as it finds it on start as applied.
	time.Sleep(100 * time.Millisecond)
	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	write(t, path, signIn+"egress: { allowHosts: [\"https://bad\"] }\n")
	wait("the refusal", func() bool { return src.LastError() != nil })
	host := func() string { return src.Current.Get().Egress.AllowHosts[0] }
	if host() != "first.example" || failing() != 1 {
		t.Fatalf("host = %s, gauge = %v; want the last good configuration running and the refusal reported", host(), failing())
	}

	write(t, path, "egress: { allowHosts: [second.example] }\n")
	wait("the sign-in refusal", func() bool { return errors.Is(src.LastError(), ErrNoSignIn) })
	if host() != "first.example" {
		t.Fatal("a configuration without a sign-in replaced the running one")
	}

	write(t, path, signIn+"egress: { allowHosts: [third.example] }\n")
	wait("the reload", func() bool { return host() == "third.example" })
	if src.LastError() != nil || failing() != 0 {
		t.Fatalf("last error = %v, gauge = %v; want both cleared", src.LastError(), failing())
	}
}

func TestLoadRefusesNoSignIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kritik.yaml")
	write(t, path, "egress: { allowHosts: [first.example] }\n")
	if _, err := (&Source{RequireSignIn: true}).Load(path); !errors.Is(err, ErrNoSignIn) {
		t.Fatalf("Load = %v, want ErrNoSignIn", err)
	}
	if _, err := (&Source{}).Load(path); err != nil {
		t.Fatalf("Load without RequireSignIn = %v", err)
	}
}
