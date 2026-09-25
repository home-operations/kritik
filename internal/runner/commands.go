package runner

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/agent"
)

// commandTool is the run tool for the commands p allows that this image
// has on its PATH, over a checkout of tree written to scratch space. It is
// nil, with a no-op cleanup, when there is nothing to offer or the
// commands cannot be kept from the runner's secrets; the review then goes
// on with the read-only tools alone. cleanup removes the scratch space.
func commandTool(ctx context.Context, p Spec, tree *agent.Tree, maxOutput int, logger *slog.Logger) (run *agent.RunTool, cleanup func()) {
	cleanup = func() {}
	found := map[string]string{}
	for _, name := range p.Agent.Commands {
		path, err := exec.LookPath(name)
		if err != nil {
			logger.Info("command not offered: not on this image's PATH", "command", name)
			continue
		}
		found[name] = path
	}
	if len(found) == 0 {
		return nil, cleanup
	}
	if err := hideEnviron(); err != nil {
		logger.Warn("commands not offered: the runner's environment cannot be hidden from them", "error", err)
		return nil, cleanup
	}
	scratch, err := os.MkdirTemp("", "kritik-run-")
	if err != nil {
		logger.Warn("commands not offered: no scratch space", "error", err)
		return nil, cleanup
	}
	cleanup = func() { _ = os.RemoveAll(scratch) }
	// HOME is apart from the checkout, or a repository could plant the
	// ~/.curlrc curl reads before its arguments.
	dir, home := filepath.Join(scratch, "checkout"), filepath.Join(scratch, "home")
	for _, d := range []string{dir, home} {
		if err := os.Mkdir(d, 0o755); err != nil {
			logger.Warn("commands not offered: no scratch space", "error", err)
			return nil, cleanup
		}
	}
	started := time.Now()
	stats, err := tree.Checkout(ctx, dir, agent.MaxCheckoutBytes)
	if err != nil {
		logger.Warn("commands not offered: checkout failed", "error", err)
		return nil, cleanup
	}
	logger.Info("checkout written", "files", stats.Files, "bytes", stats.Bytes, "skipped", stats.Skipped,
		"truncated", stats.Truncated, "elapsed", time.Since(started).Round(time.Millisecond))
	note := "The checkout leaves out ignored paths, symlinks and files over 1 MiB."
	if stats.Truncated {
		note += fmt.Sprintf(" It stopped at %d MiB, so the paths that sort last are missing.", agent.MaxCheckoutBytes>>20)
	}
	env, proxied := commandEnv(home)
	return agent.NewRunTool(agent.RunConfig{
		Dir: dir, Env: env, Commands: found, Timeout: time.Duration(p.Agent.CommandTimeoutSeconds) * time.Second,
		MaxOutputBytes: maxOutput, Proxied: proxied, Note: note,
	}), cleanup
}

// commandEnv is the whole environment of a command: PATH, home as HOME,
// and the pod's proxy settings under both spellings, since curl reads
// http_proxy only in lower case. proxied reports whether there is an
// HTTPS proxy, the egress gateway.
func commandEnv(home string) (env []string, proxied bool) {
	env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		lower := strings.ToLower(name)
		if v := cmp.Or(os.Getenv(name), os.Getenv(lower)); v != "" {
			env = append(env, name+"="+v, lower+"="+v)
			proxied = proxied || name == "HTTPS_PROXY"
		}
	}
	return env, proxied
}
