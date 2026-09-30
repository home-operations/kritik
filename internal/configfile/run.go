package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/home-operations/kritik/internal/jobtimeout"
)

// Run is how kritik runs rather than how it reviews, set by the
// environment and so by the chart's values, not the configuration file
// (ADR-0021 §2.7). A restart, not a reload, changes it.
type Run struct {
	// PollInterval is how often the leader lists each app's open pull
	// requests, its backstop for missed webhooks; 0s turns polling off.
	PollInterval time.Duration `env:"KRITIK_POLL_INTERVAL" envDefault:"10m"`
	// PollLookback bounds how far back a first or long-idle poll looks, so a
	// long outage does not list every open pull request's history at once.
	PollLookback time.Duration `env:"KRITIK_POLL_LOOKBACK" envDefault:"24h"`
	// OnboardWindow is how many onboarding index jobs the leader keeps
	// queued or running at once; accounts take turns, and the repositories
	// whose pull requests moved last go first.
	OnboardWindow int `env:"KRITIK_ONBOARD_WINDOW" envDefault:"4"`
	// IndexGrace is how long the index of a repository that stopped running
	// is kept, so turning it back on within the window reuses the index.
	IndexGrace time.Duration `env:"KRITIK_INDEX_GRACE" envDefault:"720h"`
	// TranscriptRetention is how long an agentic review's transcript is
	// kept; at least a day, since members read it after the review.
	TranscriptRetention time.Duration `env:"KRITIK_TRANSCRIPT_RETENTION" envDefault:"720h"`
	// DiffRetention is how long a review keeps the diff it was made from,
	// the context it read and the repository files it named; at least a
	// day, since members read them after the review. The review and its
	// findings stay.
	DiffRetention time.Duration `env:"KRITIK_DIFF_RETENTION" envDefault:"720h"`
	// RunnerDeadline bounds a runner Job, and RunnerResources are copied
	// verbatim into its pod spec, a JSON object of requests and limits.
	RunnerDeadline  time.Duration `env:"KRITIK_RUNNER_DEADLINE" envDefault:"15m"`
	RunnerResources jsonObject    `env:"KRITIK_RUNNER_RESOURCES"`
	// Tools are the command-line tools a runner pod may mount from an
	// image for the agent's run tool (ADR-0011), a JSON list.
	Tools toolList `env:"KRITIK_RUNNER_TOOLS"`
}

// jsonObject is a JSON object from the environment, kept as loose maps
// because the Kubernetes types are not a dependency of this package.
type jsonObject map[string]any

// UnmarshalText decodes the variable's JSON.
func (o *jsonObject) UnmarshalText(b []byte) error { return json.Unmarshal(b, (*map[string]any)(o)) }

// toolList is the tool catalog from the environment.
type toolList []Tool

// UnmarshalText decodes the variable's JSON, refusing unknown keys.
func (t *toolList) UnmarshalText(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode((*[]Tool)(t))
}

// loadRun reads Run from the environment and checks it.
func loadRun() (Run, error) {
	r, err := env.ParseAs[Run]()
	if pe, ok := errors.AsType[env.ParseError](err); ok {
		f, _ := reflect.TypeFor[Run]().FieldByName(pe.Name)
		return Run{}, fmt.Errorf("configfile: environment variable %s: %w", f.Tag.Get("env"), pe.Err)
	}
	if err != nil {
		return Run{}, fmt.Errorf("configfile: %w", err)
	}
	switch {
	case r.PollInterval < 0 || r.PollLookback < 0:
		return Run{}, errors.New("configfile: KRITIK_POLL_INTERVAL and KRITIK_POLL_LOOKBACK must not be negative")
	case r.OnboardWindow < 1:
		return Run{}, errors.New("configfile: KRITIK_ONBOARD_WINDOW must be positive")
	case r.IndexGrace < 0:
		return Run{}, errors.New("configfile: KRITIK_INDEX_GRACE must not be negative")
	case r.TranscriptRetention < minRetention:
		return Run{}, fmt.Errorf("configfile: KRITIK_TRANSCRIPT_RETENTION must be at least %s", minRetention)
	case r.DiffRetention < minRetention:
		return Run{}, fmt.Errorf("configfile: KRITIK_DIFF_RETENTION must be at least %s", minRetention)
	case r.RunnerDeadline <= 0:
		return Run{}, errors.New("configfile: KRITIK_RUNNER_DEADLINE must be positive")
	case r.RunnerDeadline > jobtimeout.MaxRunnerDeadline:
		return Run{}, fmt.Errorf("configfile: KRITIK_RUNNER_DEADLINE must not exceed %s, "+
			"or River's %s job timeout cap would cut the runner off early", jobtimeout.MaxRunnerDeadline, jobtimeout.MaxJobTimeout)
	}
	return r, validateTools(r.Tools)
}

// PollInterval is how often the leader polls, 0 when polling is off.
func (f *File) PollInterval() time.Duration { return f.Run.PollInterval }

// PollLookback bounds how far back a first or long-idle poll looks.
func (f *File) PollLookback() time.Duration { return f.Run.PollLookback }

// OnboardWindow is how many onboarding index jobs may be queued or running.
func (f *File) OnboardWindow() int { return f.Run.OnboardWindow }

// DisabledIndexGrace is how long the index of a repository that stopped
// running is kept.
func (f *File) DisabledIndexGrace() time.Duration { return f.Run.IndexGrace }

// TranscriptRetention is how long an agentic review's transcript is kept.
func (f *File) TranscriptRetention() time.Duration { return f.Run.TranscriptRetention }

// DiffRetention is how long a review's diff, context and repository files
// are kept.
func (f *File) DiffRetention() time.Duration { return f.Run.DiffRetention }

// RunnerFor resolves a runner Job's deadline and resources.
func (f *File) RunnerFor() (deadline time.Duration, resources map[string]any) {
	return f.Run.RunnerDeadline, f.Run.RunnerResources
}

// ToolsFor returns the tools that provide any of commands, the ones a run
// whose agent may run commands mounts; nil when none does.
func (f *File) ToolsFor(commands []string) []Tool {
	var out []Tool
	for _, t := range f.Run.Tools {
		if slices.ContainsFunc(t.Provides(), func(c string) bool { return slices.Contains(commands, c) }) {
			out = append(out, t)
		}
	}
	return out
}
