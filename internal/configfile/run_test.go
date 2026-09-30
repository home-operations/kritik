package configfile

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/home-operations/kritik/internal/jobtimeout"
)

// TestRun checks how kritik runs comes from the environment: its defaults,
// values set, an interval of 0s that turns polling off, and values refused.
func TestRun(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")

	t.Run("defaults when unset", func(t *testing.T) {
		f := mustLoad(t, minimal)
		deadline, resources := f.RunnerFor()
		if f.PollInterval() != 10*time.Minute || f.PollLookback() != 24*time.Hour || f.OnboardWindow() != 4 ||
			f.DisabledIndexGrace() != 30*24*time.Hour || f.TranscriptRetention() != 30*24*time.Hour || f.DiffRetention() != 30*24*time.Hour ||
			deadline != DefaultRunnerDeadline || resources != nil || f.Run.Tools != nil {
			t.Fatalf("run = %+v", f.Run)
		}
	})

	t.Run("set values, and an interval of 0s turns polling off", func(t *testing.T) {
		for k, v := range map[string]string{
			"KRITIK_POLL_INTERVAL": "0s", "KRITIK_POLL_LOOKBACK": "1h", "KRITIK_ONBOARD_WINDOW": "8", "KRITIK_INDEX_GRACE": "48h",
			"KRITIK_TRANSCRIPT_RETENTION": "72h", "KRITIK_DIFF_RETENTION": "96h", "KRITIK_RUNNER_DEADLINE": "10m",
			"KRITIK_RUNNER_RESOURCES": `{"limits":{"memory":"1Gi"}}`,
		} {
			t.Setenv(k, v)
		}
		f := mustLoad(t, minimal)
		deadline, resources := f.RunnerFor()
		if f.PollInterval() != 0 || f.PollLookback() != time.Hour || f.OnboardWindow() != 8 || f.DisabledIndexGrace() != 48*time.Hour ||
			f.TranscriptRetention() != 72*time.Hour || f.DiffRetention() != 96*time.Hour || deadline != 10*time.Minute || resources["limits"] == nil {
			t.Fatalf("run = %+v", f.Run)
		}
	})

	refused := []struct{ env, value, want string }{
		{"KRITIK_POLL_INTERVAL", "-1m", "must not be negative"},
		{"KRITIK_ONBOARD_WINDOW", "0", "KRITIK_ONBOARD_WINDOW must be positive"},
		{"KRITIK_TRANSCRIPT_RETENTION", "1h", "KRITIK_TRANSCRIPT_RETENTION must be at least 24h"},
		{"KRITIK_DIFF_RETENTION", "1h", "KRITIK_DIFF_RETENTION must be at least 24h"},
		{"KRITIK_RUNNER_DEADLINE", fmt.Sprintf("%ds", int64(jobtimeout.MaxRunnerDeadline.Seconds())+1), "KRITIK_RUNNER_DEADLINE must not exceed"},
		{"KRITIK_RUNNER_RESOURCES", "[1]", "KRITIK_RUNNER_RESOURCES"},
		{"KRITIK_POLL_LOOKBACK", "soon", "KRITIK_POLL_LOOKBACK"},
	}
	for _, tt := range refused {
		t.Run(tt.env+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.env, tt.value)
			if _, err := Parse([]byte(minimal)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}
	t.Run("a deadline at the cap", func(t *testing.T) {
		t.Setenv("KRITIK_RUNNER_DEADLINE", jobtimeout.MaxRunnerDeadline.String())
		mustLoad(t, minimal)
	})
}

func TestTools(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", "tok")
	t.Setenv("TEST_WEBHOOK_SECRET", "whsec")
	t.Setenv("KRITIK_RUNNER_TOOLS", `[{"name": "helm", "image": "registry.example/helm:3", "path": "/usr/bin"},
		{"name": "flux-tools", "image": "registry.example/flux:2", "commands": ["flux", "flate"]}]`)
	f := mustLoad(t, minimal)
	names := func(ts []Tool) []string {
		out := make([]string, 0, len(ts))
		for _, x := range ts {
			out = append(out, x.Name)
		}
		return out
	}
	if got := names(f.ToolsFor([]string{"curl", "flate"})); !slices.Equal(got, []string{"flux-tools"}) {
		t.Fatalf("ToolsFor(curl, flate) = %v, want flux-tools", got)
	}
	if got := names(f.ToolsFor([]string{"helm", "flux"})); !slices.Equal(got, []string{"helm", "flux-tools"}) {
		t.Fatalf("ToolsFor(helm, flux) = %v", got)
	}
	if got := f.ToolsFor([]string{"curl", "rg"}); got != nil {
		t.Fatalf("ToolsFor(curl, rg) = %v, want nil: the runner image provides those", got)
	}

	refused := map[string]string{
		`{"name": "Helm", "image": "x"}`:                                 "must be lowercase",
		`{"name": "helm", "image": "x"}, {"name": "helm", "image": "y"}`: `"helm" is listed twice`,
		`{"name": "helm"}`: "KRITIK_RUNNER_TOOLS[0].image is required",
		`{"name": "helm", "image": "x", "path": "usr/bin"}`:                                     "must be a clean absolute path",
		`{"name": "helm", "image": "x", "path": "/usr/../etc"}`:                                 "must be a clean absolute path",
		`{"name": "helm", "image": "x", "commands": ["bin/helm"]}`:                              "must be a bare command name",
		`{"name": "helm", "image": "x"}, {"name": "helm2", "image": "y", "commands": ["helm"]}`: `which tool "helm" already provides`,
		`{"name": "helm", "image": "x", "binaries": ["helm"]}`:                                  "unknown field",
	}
	for list, want := range refused {
		t.Run(list, func(t *testing.T) {
			t.Setenv("KRITIK_RUNNER_TOOLS", "["+list+"]")
			if _, err := Parse([]byte(minimal)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, want)
			}
		})
	}
}
