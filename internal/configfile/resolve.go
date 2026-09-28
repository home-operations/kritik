package configfile

import (
	"reflect"
	"slices"
	"strings"
	"time"
)

// Account returns the running account name on forge.
func (f *File) Account(forge Forge, name string) (*Account, bool) {
	key := AccountKey(forge, name)
	for i := range f.Accounts {
		if f.Accounts[i].Key() == key {
			return &f.Accounts[i], true
		}
	}
	return nil, false
}

// AccountByID returns the running account whose id is id.
func (f *File) AccountByID(id string) (*Account, bool) {
	for i := range f.Accounts {
		if f.Accounts[i].ID() == id {
			return &f.Accounts[i], true
		}
	}
	return nil, false
}

// Connection returns the running connection with the given name. Names are
// unique, so this is how the ingest role turns a hook path into a webhook
// secret.
func (f *File) Connection(name string) (*Connection, bool) {
	for i := range f.Connections {
		if f.Connections[i].Name == name {
			return &f.Connections[i], true
		}
	}
	return nil, false
}

// ConnectionFor returns the running connection serving a, nil when none
// does.
func (f *File) ConnectionFor(a *Account) *Connection {
	for i := range f.Connections {
		if in := &f.Connections[i]; in.Forge == a.Forge && in.Serves(a.Name) {
			return in
		}
	}
	return nil
}

// Repository returns the account's entry for the repository fullName,
// "owner/repo", nil when it lists none.
func (a *Account) Repository(fullName string) *Repository {
	for i := range a.Repositories {
		if r := &a.Repositories[i]; strings.EqualFold(a.Name+"/"+r.Name, fullName) {
			return r
		}
	}
	return nil
}

// Settings resolves the effective settings for the repository fullName,
// "owner/repo", of account a. Layers apply in one direction: defaults, then
// the account, then its entry for the repository, if one exists. A
// repository the account does not list gets the account's settings and is
// enabled; an empty fullName gives the account's settings alone.
func (f *File) Settings(a *Account, fullName string) Settings {
	s := Settings{
		Enabled:     true,
		Ignore:      append([]string(nil), DefaultIgnore...),
		Mode:        ReviewSingle,
		Agent:       DefaultAgent,
		Incremental: IncrementalSettings{MaxDeltaFiles: DefaultMaxDeltaFiles},
		Review:      Review{InlineComments: true},
	}
	s.apply(&f.Defaults.Overrides)
	s.Limits = s.Limits.overlay(f.Defaults.Limits)
	s.apply(&a.Overrides)
	s.Limits = s.Limits.overlay(a.Limits)
	if r := a.Repository(fullName); r != nil {
		if r.Enabled != nil {
			s.Enabled = *r.Enabled
		}
		s.apply(&r.Overrides)
	}
	if s.Limits.Concurrency == 0 {
		s.Limits.Concurrency = DefaultConcurrency
	}
	return s
}

// Source is the layer a setting's value comes from.
type Source string

// Sources of a setting.
const (
	// SourceDefault is kritik's built-in default.
	SourceDefault Source = "default"
	SourceEnv     Source = "env"
	SourceFile    Source = "file"
	// SourceDashboard is the instance spec, SourceDefaults its defaults and
	// SourceAccount an account's entry or one of its repository entries.
	SourceDashboard  Source = "dashboard"
	SourceDefaults   Source = "defaults"
	SourceAccount    Source = "account"
	SourceRepository Source = "repository"
)

// Sources says, for each setting the policy table lets an admin write,
// where the settings Settings resolves for the same repository take it
// from: the narrowest scope that writes it, or the built-in default.
// Ignore globs come from every scope; the narrowest that adds some is
// given.
func (f *File) Sources(a *Account, fullName string) map[string]Source {
	type scope struct {
		spec   any
		source Source
	}
	scopes := []scope{{&f.Defaults, SourceDefaults}, {a, SourceAccount}}
	if r := a.Repository(fullName); r != nil {
		scopes = append(scopes, scope{r, SourceAccount})
	}
	out := map[string]Source{}
	for _, p := range Policies {
		if len(p.Scopes) == 0 {
			continue
		}
		out[p.Key] = SourceDefault
		for _, sc := range scopes {
			if v, ok := SpecValue(sc.spec, p.Key); ok && !reflect.ValueOf(v).IsZero() {
				out[p.Key] = sc.source
			}
		}
	}
	return out
}

// apply lays one scope's overrides over s: a field the scope writes
// replaces s's, and its ignore globs are added to s's.
func (s *Settings) apply(o *Overrides) {
	s.Models = s.Models.overlay(o.Models)
	if o.Filter != nil {
		s.Filter = o.filter
	}
	if o.Forks != nil {
		s.Forks = *o.Forks
	}
	s.Ignore = append(s.Ignore, o.Ignore...)
	if o.Settle != nil {
		s.Settle = *o.Settle
	}
	if o.Mode != "" {
		s.Mode = o.Mode
	}
	s.Agent = s.Agent.overlay(o.Agent)
	if o.Incremental.MaxDeltaFiles != nil {
		s.Incremental.MaxDeltaFiles = *o.Incremental.MaxDeltaFiles
	}
	s.Review = s.Review.overlay(o.Review)
	s.Allow = s.Allow.overlay(o.Allow)
}

// PollInterval is how often the leader polls, 0 when polling is off.
func (f *File) PollInterval() time.Duration {
	if f.Polling.Interval != nil {
		return *f.Polling.Interval
	}
	return DefaultPollInterval
}

// PollLookback bounds how far back a first or long-idle poll looks.
func (f *File) PollLookback() time.Duration {
	if f.Polling.Lookback > 0 {
		return f.Polling.Lookback
	}
	return DefaultPollLookback
}

// ToolsFor returns the tools that provide any of commands, the ones a run
// whose agent may run commands mounts; nil when none does.
func (f *File) ToolsFor(commands []string) []Tool {
	var out []Tool
	for _, t := range f.Tools {
		if slices.ContainsFunc(t.Provides(), func(c string) bool { return slices.Contains(commands, c) }) {
			out = append(out, t)
		}
	}
	return out
}

// OnboardWindow is how many onboarding index jobs may be queued or running.
func (f *File) OnboardWindow() int {
	if f.Indexing.OnboardWindow > 0 {
		return f.Indexing.OnboardWindow
	}
	return DefaultOnboardWindow
}

// RunnerFor resolves an account's runner Job deadline and resources: the
// account's runner block, then defaults.runner, then DefaultRunnerDeadline
// and no resources. t may be nil for an account no longer served.
func (f *File) RunnerFor(t *Account) (deadline time.Duration, resources map[string]any) {
	deadline = DefaultRunnerDeadline
	blocks := []*Runner{f.Defaults.Runner}
	if t != nil {
		blocks = append(blocks, t.Runner)
	}
	for _, r := range blocks {
		if r == nil {
			continue
		}
		if r.ActiveDeadlineSeconds > 0 {
			deadline = time.Duration(r.ActiveDeadlineSeconds) * time.Second
		}
		if r.Resources != nil {
			resources = r.Resources
		}
	}
	return deadline, resources
}

// DisabledIndexGrace returns the configured grace period or the default.
func (f *File) DisabledIndexGrace() time.Duration {
	if f.Retention.DisabledIndexGrace > 0 {
		return f.Retention.DisabledIndexGrace
	}
	return DefaultDisabledIndexGrace
}

func (m Models) overlay(o ModelsSpec) Models {
	if o.Review != nil {
		m.Review = *o.Review
	}
	if o.Fallback != nil {
		m.Fallback = *o.Fallback
	}
	return m
}

func (l Limits) overlay(o LimitsSpec) Limits {
	if o.Concurrency != nil {
		l.Concurrency = *o.Concurrency
	}
	if o.ReviewsPerDay != nil {
		l.ReviewsPerDay = *o.ReviewsPerDay
	}
	if o.TokensPerMonth != nil {
		l.TokensPerMonth = *o.TokensPerMonth
	}
	return l
}

func (r Review) overlay(o ReviewSpec) Review {
	if o.Instructions != nil {
		r.Instructions = o.Instructions
	}
	if o.RequireSuggestedFix != nil {
		r.RequireSuggestedFix = *o.RequireSuggestedFix
	}
	if o.Templates.Summary != nil {
		r.Templates.Summary = *o.Templates.Summary
	}
	if o.Templates.Inline != nil {
		r.Templates.Inline = *o.Templates.Inline
	}
	if o.MinSeverity != nil {
		r.MinSeverity = *o.MinSeverity
	}
	if o.InlineComments != nil {
		r.InlineComments = *o.InlineComments
	}
	if o.Context != nil {
		r.Context = o.Context
	}
	return r
}

func (a Allow) overlay(o Allow) Allow {
	if o.Modes != nil {
		a.Modes = o.Modes
	}
	if o.Models != nil {
		a.Models = o.Models
	}
	if o.Commands != nil {
		a.Commands = o.Commands
	}
	if o.Agent.MaxSteps != nil {
		a.Agent.MaxSteps = o.Agent.MaxSteps
	}
	if o.Agent.MaxToolOutputBytes != nil {
		a.Agent.MaxToolOutputBytes = o.Agent.MaxToolOutputBytes
	}
	if o.Agent.MaxTokens != nil {
		a.Agent.MaxTokens = o.Agent.MaxTokens
	}
	if o.Agent.Timeout != nil {
		a.Agent.Timeout = o.Agent.Timeout
	}
	if o.Settle != nil {
		a.Settle = o.Settle
	}
	return a
}

func (a AgentSettings) overlay(o Agent) AgentSettings {
	if o.MaxSteps != nil {
		a.MaxSteps = *o.MaxSteps
	}
	if o.MaxToolOutputBytes != nil {
		a.MaxToolOutputBytes = *o.MaxToolOutputBytes
	}
	if o.MaxTokens != nil {
		a.MaxTokens = *o.MaxTokens
	}
	if o.Timeout != nil {
		a.Timeout = *o.Timeout
	}
	if o.Commands != nil {
		a.Commands = o.Commands
	}
	if o.CommandTimeout != nil {
		a.CommandTimeout = *o.CommandTimeout
	}
	return a
}
