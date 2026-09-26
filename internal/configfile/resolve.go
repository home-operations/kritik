package configfile

import (
	"strings"
	"time"
)

// Tenant returns the tenant with the given slug.
func (f *File) Tenant(slug string) (*Tenant, bool) {
	for i := range f.Tenants {
		if f.Tenants[i].Slug == slug {
			return &f.Tenants[i], true
		}
	}
	return nil, false
}

// Installation returns the installation with the given name and the tenant
// that owns it. Names are unique across the file, so this is how the ingest
// role turns a hook path into a webhook secret.
func (f *File) Installation(name string) (*Installation, *Tenant, bool) {
	for ti := range f.Tenants {
		t := &f.Tenants[ti]
		for ii := range t.Installations {
			if t.Installations[ii].Name == name {
				return &t.Installations[ii], t, true
			}
		}
	}
	return nil, nil, false
}

// InstallationFor returns the tenant's installation whose account owns the
// repository "owner/repo", or nil. A tenant with several installations on
// different forges may share an account name; the first declared wins,
// which the operator controls by ordering.
func (f *File) InstallationFor(t *Tenant, repo string) *Installation {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return nil
	}
	for i := range t.Installations {
		if strings.EqualFold(t.Installations[i].Account, owner) {
			return &t.Installations[i]
		}
	}
	return nil
}

// Settings resolves the effective settings for a repository of a tenant.
// Layers apply in one direction: defaults, then the tenant, then the
// repository entry if one exists. A repository not listed under the tenant
// gets the tenant's settings and is enabled.
func (f *File) Settings(t *Tenant, repo string) Settings {
	s := Settings{
		Enabled: true,
		Models:  f.Defaults.Models,
		Filter:  f.defaultFilter,
		Forks:   f.Defaults.Forks != nil && *f.Defaults.Forks,
		Limits:  f.Defaults.Limits,
		Ignore:  append([]string(nil), DefaultIgnore...),
		Settle:  f.Defaults.Settle,

		Mode:        ReviewSingle,
		Agent:       DefaultAgent,
		Incremental: IncrementalSettings{MaxDeltaFiles: DefaultMaxDeltaFiles},
	}
	s.Models = s.Models.overlay(t.Models)
	if t.filter != nil {
		s.Filter = t.filter
	}
	if t.Forks != nil {
		s.Forks = *t.Forks
	}
	if t.Settle > 0 {
		s.Settle = t.Settle
	}
	s.Limits = s.Limits.overlay(t.Limits)
	for i := range t.Repositories {
		r := &t.Repositories[i]
		if r.Name != repo {
			continue
		}
		if r.Enabled != nil {
			s.Enabled = *r.Enabled
		}
		if r.filter != nil {
			s.Filter = r.filter
		}
		s.Konflate = r.Konflate
		s.Ignore = append(s.Ignore, r.Ignore...)
		if r.Settle > 0 {
			s.Settle = r.Settle
		}
		if r.Mode != "" {
			s.Mode = r.Mode
		}
		s.Agent = s.Agent.overlay(r.Agent)
		if r.Incremental.MaxDeltaFiles != nil {
			s.Incremental.MaxDeltaFiles = *r.Incremental.MaxDeltaFiles
		}
		s.Review = r.Review
		break
	}
	if s.Limits.Concurrency == 0 {
		s.Limits.Concurrency = DefaultConcurrency
	}
	return s
}

// DisabledIndexGrace returns the configured grace period or the default.
func (f *File) DisabledIndexGrace() time.Duration {
	if f.Retention.DisabledIndexGrace > 0 {
		return f.Retention.DisabledIndexGrace
	}
	return DefaultDisabledIndexGrace
}

func (m Models) overlay(o Models) Models {
	if o.Review != "" {
		m.Review = o.Review
	}
	if o.Fallback != "" {
		m.Fallback = o.Fallback
	}
	return m
}

func (l Limits) overlay(o Limits) Limits {
	if o.Concurrency != 0 {
		l.Concurrency = o.Concurrency
	}
	if o.ReviewsPerDay != 0 {
		l.ReviewsPerDay = o.ReviewsPerDay
	}
	if o.TokensPerMonth != 0 {
		l.TokensPerMonth = o.TokensPerMonth
	}
	return l
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
