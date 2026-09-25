package runner

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/home-operations/kritik/internal/agent"
)

// environHelper names the role the test binary plays when TestHideEnviron
// runs it again: "keep" or "hide" start a reader and report what it saw,
// "read" is the reader.
const environHelper = "KRITIK_TEST_ENVIRON"

// TestHideEnviron checks the property the run tool relies on: once the
// runner is non-dumpable, a child with the runner's uid cannot read the
// runner's environment from /proc.
func TestHideEnviron(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	switch mode := os.Getenv(environHelper); mode {
	case "read":
		if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", os.Getppid())); err != nil {
			fmt.Print("denied")
		} else {
			fmt.Print("readable")
		}
		os.Exit(0)
	case "keep", "hide":
		if mode == "hide" {
			if err := hideEnviron(); err != nil {
				fmt.Print("hideEnviron: ", err)
				os.Exit(0)
			}
		}
		reader := exec.Command(self, "-test.run=^TestHideEnviron$")
		reader.Env = append(os.Environ(), environHelper+"=read")
		out, _ := reader.Output()
		fmt.Print(string(out))
		os.Exit(0)
	}
	for mode, want := range map[string]string{"keep": "readable", "hide": "denied"} {
		cmd := exec.Command(self, "-test.run=^TestHideEnviron$")
		cmd.Env = append(os.Environ(), environHelper+"="+mode)
		out, err := cmd.Output()
		if err != nil || string(out) != want {
			t.Fatalf("%s: a child read the parent's environ: %q, %v; want %q", mode, out, err, want)
		}
	}
}

func TestCommandTool(t *testing.T) {
	// commandTool hides the environment of the process it runs in, this
	// test binary; make it dumpable again afterwards.
	t.Cleanup(func() { _, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 1, 0) })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(self, filepath.Join(bin, "rg")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HTTPS_PROXY", "http://gateway:8082")
	t.Setenv("TMPDIR", t.TempDir())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	head := agent.NewTree(tree(t, map[string]string{"main.go": "package main\n", "vendor/x.go": "package x\n"}), []string{"vendor/**"})

	t.Run("offers what is on PATH, over a checkout", func(t *testing.T) {
		s := agenticSpec()
		s.Agent.Commands, s.Agent.CommandTimeoutSeconds = []string{"curl", "rg"}, 5
		run, cleanup := commandTool(t.Context(), s, head, 1024, logger)
		if run == nil {
			t.Fatal("no run tool")
		}
		if !slices.Equal(run.Names(), []string{"rg"}) {
			t.Fatalf("names = %v", run.Names())
		}
		def := run.Def()
		if !strings.Contains(def.Description, "leaves out ignored paths") || !strings.Contains(def.Description, "after 5s") {
			t.Fatalf("description = %q", def.Description)
		}
		scratch, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "kritik-run-*"))
		if err != nil || len(scratch) != 1 {
			t.Fatalf("scratch = %v, %v", scratch, err)
		}
		if _, err := os.Stat(filepath.Join(scratch[0], "checkout", "main.go")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(scratch[0], "checkout", "vendor")); !os.IsNotExist(err) {
			t.Fatalf("ignored path checked out: %v", err)
		}
		cleanup()
		if _, err := os.Stat(scratch[0]); !os.IsNotExist(err) {
			t.Fatalf("scratch left behind: %v", err)
		}
	})

	t.Run("nothing to offer", func(t *testing.T) {
		for _, commands := range [][]string{nil, {"curl"}} {
			s := agenticSpec()
			s.Agent.Commands, s.Agent.CommandTimeoutSeconds = commands, 5
			run, cleanup := commandTool(t.Context(), s, head, 1024, logger)
			cleanup()
			if run != nil {
				t.Fatalf("%v: run tool offered with %v", commands, run.Names())
			}
		}
	})
}
