package configfile

import (
	"cmp"
	"maps"
	"reflect"
	"slices"
	"strings"
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
// repository the account does not list gets the account's settings, so it
// is enabled unless the defaults or the account turn it off; an empty
// fullName gives the account's settings alone.
func (f *File) Settings(a *Account, fullName string) Settings {
	s := Settings{
		Enabled:     true,
		Ignore:      append([]string(nil), DefaultIgnore...),
		Mode:        ReviewAgentic,
		Agent:       DefaultAgent,
		Incremental: IncrementalSettings{MaxDeltaFiles: DefaultMaxDeltaFiles},
		Review:      Review{InlineComments: true, Feedback: FeedbackDetailed, AgentFiles: true},
		Providers:   f.providerNames(a),
	}
	s.apply(&f.Defaults.Overrides)
	s.Limits = s.Limits.overlay(f.Defaults.Limits)
	s.apply(&a.Overrides)
	s.Limits = s.Limits.overlay(a.Limits)
	if r := a.Repository(fullName); r != nil {
		s.apply(&r.Overrides)
	}
	s.Limits.Concurrency = cmp.Or(s.Limits.Concurrency, DefaultConcurrency)
	return s
}

// Runs reports whether repository fullName of account a, known as t, is
// reviewed, polled and indexed (ADR-0019 §2.3). An archived repository
// never is: it is read-only until unarchived. One an admin turned on or off
// runs as they chose. A fork does not otherwise, since an account can reach
// many forks it never meant kritik to spend on. Any other repository runs
// as its settings say.
func (f *File) Runs(a *Account, fullName string, t RepoTraits) bool {
	switch {
	case t.Archived:
		return false
	case t.TurnedOn != nil:
		return *t.TurnedOn
	case t.Fork:
		return false
	default:
		return f.Settings(a, fullName).Enabled
	}
}

// Source is the layer a setting's value comes from.
type Source string

// Sources of a setting.
const (
	// SourceDefault is kritik's built-in default.
	SourceDefault Source = "default"
	SourceEnv     Source = "env"
	SourceFile    Source = "file"
	// SourceDefaults is the file's defaults and SourceAccount an account's
	// entry or one of its repository entries.
	SourceDefaults Source = "defaults"
	SourceAccount  Source = "account"
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
		if out[p.Key] == SourceDefaults && f.envKeys[p.Key] {
			out[p.Key] = SourceEnv
		}
	}
	return out
}

// apply lays one scope's overrides over s: a field the scope writes
// replaces s's, and its ignore globs are added to s's.
func (s *Settings) apply(o *Overrides) {
	if o.Enabled != nil {
		s.Enabled = *o.Enabled
	}
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
}

// providerNames is the names of the providers account a may use, sorted.
func (f *File) providerNames(a *Account) []string {
	names := slices.Collect(maps.Keys(f.Providers))
	names = append(names, slices.Collect(maps.Keys(a.Providers))...)
	slices.Sort(names)
	return names
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
	if o.RequireSuggestedFix != nil {
		r.RequireSuggestedFix = *o.RequireSuggestedFix
	}
	if o.Comments.SummaryTemplate != nil {
		r.Templates.Summary = *o.Comments.SummaryTemplate
	}
	if o.Comments.InlineTemplate != nil {
		r.Templates.Inline = *o.Comments.InlineTemplate
	}
	if o.Comments.Inline != nil {
		r.InlineComments = *o.Comments.Inline
	}
	if o.Context != nil {
		r.Context = o.Context
	}
	r.Rules = WithRules(r.Rules, o.Rules)
	if o.AgentFiles != nil {
		r.AgentFiles = *o.AgentFiles
	}
	if o.Feedback != nil {
		r.Feedback = *o.Feedback
	}
	return r
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
