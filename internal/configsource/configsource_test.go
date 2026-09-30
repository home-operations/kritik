package configsource

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const signIn = "auth: { admin: { password: { env: TEST_ADMIN_PASSWORD } } }\n"

func write(t *testing.T, path, doc string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadSeedsCurrent: the configuration Load reads is the running one.
func TestLoadSeedsCurrent(t *testing.T) {
	t.Setenv("TEST_ADMIN_PASSWORD", "pw")
	path := filepath.Join(t.TempDir(), "kritik.yaml")
	write(t, path, signIn+"egress: { allowHosts: [first.example] }\n")
	src := &Source{RequireSignIn: true}
	if _, err := src.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := src.Current.Get().Egress.AllowHosts; len(got) != 1 || got[0] != "first.example" {
		t.Fatalf("running allowHosts = %v", got)
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
