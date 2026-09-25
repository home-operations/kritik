package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// helperEnv makes the test binary act as a command for the run tool, so
// the tests run a real process without depending on what the host has on
// its PATH.
const helperEnv = "KRITIK_AGENT_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// helper is the command: "exit N" exits N, "sleep" outlives any test
// timeout, "flood N" writes N bytes, and anything else prints the working
// directory, the environment and the arguments.
func helper(args []string) int {
	switch {
	case len(args) == 2 && args[0] == "exit":
		n, _ := strconv.Atoi(args[1])
		fmt.Println("exiting")
		return n
	case len(args) == 1 && args[0] == "sleep":
		time.Sleep(time.Minute)
		return 0
	case len(args) == 2 && args[0] == "flood":
		n, _ := strconv.Atoi(args[1])
		fmt.Print(strings.Repeat("x", n))
		return 0
	}
	wd, _ := os.Getwd()
	fmt.Println("wd=" + wd)
	for _, e := range os.Environ() {
		fmt.Println("env=" + e)
	}
	for _, a := range args {
		fmt.Println("arg=" + a)
	}
	return 0
}

func newTestRunTool(t *testing.T, proxied bool) (*RunTool, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return NewRunTool(RunConfig{
		// A coverage build of the helper warns on exit without GOCOVERDIR.
		Dir: dir, Env: []string{helperEnv + "=1", "HOME=/nowhere", "GOCOVERDIR=" + t.TempDir()},
		Commands: map[string]string{"rg": self, "curl": self},
		Timeout:  2 * time.Second, MaxOutputBytes: 4096, Proxied: proxied,
	}), dir
}

func TestRunTool(t *testing.T) {
	rt, dir := newTestRunTool(t, false)

	t.Run("runs the binary directly in the checkout with only the given environment", func(t *testing.T) {
		out, err := rt.Run(t.Context(), json.RawMessage(`{"command":"rg","args":["a b","$HOME","*.go"]}`))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if lines[0] != "exit code 0" || !slices.Contains(lines, "arg=a b") || !slices.Contains(lines, "arg=$HOME") ||
			!slices.Contains(lines, "arg=*.go") {
			t.Fatalf("out = %q", out)
		}
		// The temp dir may sit behind a symlink (macOS's /var).
		real, _ := filepath.EvalSymlinks(dir)
		if !slices.Contains(lines, "wd="+dir) && !slices.Contains(lines, "wd="+real) {
			t.Fatalf("out = %q, want wd=%s", out, dir)
		}
		var env []string
		for _, l := range lines {
			if v, ok := strings.CutPrefix(l, "env="); ok {
				env = append(env, v)
			}
		}
		if !slices.Equal(env, rt.cfg.Env) {
			t.Fatalf("env = %q", env)
		}
	})

	t.Run("a non-zero exit is output, not an error", func(t *testing.T) {
		out, err := rt.Run(t.Context(), json.RawMessage(`{"command":"rg","args":["exit","3"]}`))
		if err != nil || out != "exit code 3\nexiting\n" {
			t.Fatalf("out = %q, err = %v", out, err)
		}
	})

	t.Run("a command not on the list is refused", func(t *testing.T) {
		if _, err := rt.Run(t.Context(), json.RawMessage(`{"command":"sh","args":["-c","id"]}`)); err == nil ||
			!strings.Contains(err.Error(), "not one of curl, rg") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("output is capped", func(t *testing.T) {
		out, err := rt.Run(t.Context(), json.RawMessage(`{"command":"rg","args":["flood","5000"]}`))
		if err != nil || !strings.HasSuffix(out, "\n[truncated 904 bytes]") || strings.Count(strings.TrimPrefix(out, "exit code 0\n"), "x") != 4096 {
			t.Fatalf("out = %.80q…, err = %v", out, err)
		}
	})

	t.Run("a command past its timeout is stopped", func(t *testing.T) {
		slow := NewRunTool(RunConfig{Dir: dir, Env: rt.cfg.Env, Commands: rt.cfg.Commands, Timeout: 100 * time.Millisecond, MaxOutputBytes: 64})
		start := time.Now()
		out, err := slow.Run(t.Context(), json.RawMessage(`{"command":"rg","args":["sleep"]}`))
		if err != nil || !strings.HasPrefix(out, "stopped after 100ms") || time.Since(start) > 5*time.Second {
			t.Fatalf("out = %q, err = %v", out, err)
		}
	})
}

func TestRunToolRecordsCurlSources(t *testing.T) {
	for _, tt := range []struct {
		name    string
		proxied bool
		want    []string
	}{
		{"direct", false, []string{
			"https://api.github.com/repos/a/b/releases/tags/v1",
			"http://example.com/a%20b?q=%3Cx%3E",
		}},
		{"through the gateway, fetched over https", true, []string{
			"https://api.github.com/repos/a/b/releases/tags/v1",
			"https://example.com/a%20b?q=%3Cx%3E",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt, _ := newTestRunTool(t, tt.proxied)
			for _, input := range []string{
				`{"command":"curl","args":["-sSL","https://api.github.com/repos/a/b/releases/tags/v1","-H","Accept: application/json"]}`,
				`{"command":"curl","args":["http://user:secret@example.com/a b?q=<x>","https://api.github.com/repos/a/b/releases/tags/v1"]}`,
				`{"command":"curl","args":["file:///proc/self/environ","ftp://example.com/x","not a url"]}`,
				// Only curl's arguments are sources.
				`{"command":"rg","args":["https://other.example.com/"]}`,
			} {
				if _, err := rt.Run(t.Context(), json.RawMessage(input)); err != nil {
					t.Fatal(err)
				}
			}
			if got := rt.Sources(); !slices.Equal(got, tt.want) {
				t.Fatalf("sources = %q, want %q", got, tt.want)
			}
		})
	}
	rt, _ := newTestRunTool(t, false)
	if got := rt.Sources(); got == nil || len(got) != 0 {
		t.Fatalf("sources of an unused tool = %#v", got)
	}
}

func TestRunToolDef(t *testing.T) {
	proxied, _ := newTestRunTool(t, true)
	def := proxied.Def()
	if def.Name != "run" || !strings.Contains(def.Description, "curl, rg") || !strings.Contains(def.Description, "Give curl http:// URLs") {
		t.Fatalf("def = %+v", def)
	}
	var schema struct {
		Properties struct {
			Command struct {
				Enum []string `json:"enum"`
			} `json:"command"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil || !slices.Equal(schema.Properties.Command.Enum, []string{"curl", "rg"}) {
		t.Fatalf("schema = %s, err = %v", def.InputSchema, err)
	}
	direct, _ := newTestRunTool(t, false)
	if d := direct.Def().Description; !strings.Contains(d, "Give curl https:// URLs") {
		t.Fatalf("description = %q", d)
	}
	noCurl := NewRunTool(RunConfig{Commands: map[string]string{"fd": "/bin/fd"}, Timeout: time.Second, Note: "The checkout is partial."})
	if d := noCurl.Def().Description; strings.Contains(d, "curl") || !strings.HasSuffix(d, " The checkout is partial.") {
		t.Fatalf("description = %q", d)
	}
}

func TestCheckout(t *testing.T) {
	t.Run("writes what the commands may read", func(t *testing.T) {
		dir := t.TempDir()
		stats, err := NewTree(testTree(t), []string{"generated/**"}).Checkout(t.Context(), dir, MaxCheckoutBytes)
		if err != nil {
			t.Fatal(err)
		}
		// generated/gen.go is ignored and huge.txt is over the size cap.
		if stats.Files != 6 || stats.Skipped != 2 || stats.Truncated {
			t.Fatalf("stats = %+v", stats)
		}
		b, err := os.ReadFile(filepath.Join(dir, "main.go"))
		if err != nil || !strings.HasPrefix(string(b), "package main") {
			t.Fatalf("main.go = %q, %v", b, err)
		}
		for _, name := range []string{"generated/gen.go", "huge.txt"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("%s written: %v", name, err)
			}
		}
		info, err := os.Stat(filepath.Join(dir, "alpha.go"))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("alpha.go: %v, %v", info, err)
		}
	})

	t.Run("symlinks are skipped", func(t *testing.T) {
		dir := t.TempDir()
		stats, err := NewTree(symlinkTree(t), nil).Checkout(t.Context(), dir, MaxCheckoutBytes)
		if err != nil || stats.Files != 1 || stats.Skipped != 1 {
			t.Fatalf("stats = %+v, err = %v", stats, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "passwd")); !os.IsNotExist(err) {
			t.Fatalf("symlink written: %v", err)
		}
	})

	t.Run("stops at the total cap", func(t *testing.T) {
		stats, err := NewTree(testTree(t), nil).Checkout(t.Context(), t.TempDir(), 100)
		if err != nil || !stats.Truncated || stats.Bytes > 100 {
			t.Fatalf("stats = %+v, err = %v", stats, err)
		}
	})
}

// symlinkTree is a commit holding a file and a symlink out of the tree.
func symlinkTree(t *testing.T) *object.Tree {
	t.Helper()
	fs := memfs.New()
	r, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := fs.Create("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("/etc/passwd", "passwd"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatal(err)
	}
	hash, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(hash)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	return tree
}
