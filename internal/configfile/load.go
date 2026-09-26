package configfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/prfilter"
)

// nameRe bounds installation and tenant names to what is safe in a URL path
// segment, a Kubernetes label value and a log line.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// commandRe is a binary name the run tool looks up on PATH: no path
// separator, so the allowlist cannot name a file in the checkout.
var commandRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// Load reads, decodes, resolves and validates the file at name.
func Load(name string) (*File, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("configfile: %w", err)
	}
	return Parse(raw)
}

// Parse is Load for bytes already in hand.
func Parse(raw []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configfile: file is empty")
		}
		return nil, fmt.Errorf("configfile: parse: %w", err)
	}
	if err := f.resolve(); err != nil {
		return nil, err
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	f.hash = hex.EncodeToString(sum[:])
	return &f, nil
}

// resolve reads every secret reference into memory and compiles every filter.
func (f *File) resolve() error {
	for name, p := range f.Providers {
		v, err := p.APIKey.resolve(fileRefs)
		if err != nil {
			return fmt.Errorf("configfile: providers.%s.apiKey: %w", name, err)
		}
		p.apiKey = v
		f.Providers[name] = p
	}

	f.Egress.credentials = make(map[string]Secret, len(f.Egress.Credentials))
	for host, ref := range f.Egress.Credentials {
		v, err := ref.resolve(fileRefs)
		if err != nil {
			return fmt.Errorf("configfile: egress.credentials.%s: %w", host, err)
		}
		f.Egress.credentials[strings.ToLower(host)] = v
	}

	if err := f.Web.resolve(); err != nil {
		return err
	}

	prg, err := compileFilter(f.Defaults.Filter)
	if err != nil {
		return fmt.Errorf("configfile: defaults.filter: %w", err)
	}
	f.defaultFilter = prg

	for ti := range f.Tenants {
		if err := f.Tenants[ti].resolve(fmt.Sprintf("tenants[%d]", ti), fileRefs); err != nil {
			return err
		}
	}
	return nil
}

// resolve reads the tenant's secret references under refs and compiles its
// filters; where prefixes every error.
func (t *Tenant) resolve(where string, refs refPolicy) error {
	var err error
	if t.filter, err = compileFilter(t.Filter); err != nil {
		return fmt.Errorf("configfile: %s.filter: %w", where, err)
	}
	for ii := range t.Installations {
		if err := t.Installations[ii].resolve(fmt.Sprintf("%s.installations[%d]", where, ii), refs); err != nil {
			return err
		}
	}
	for ri := range t.Repositories {
		r := &t.Repositories[ri]
		if r.filter, err = compileFilter(r.Filter); err != nil {
			return fmt.Errorf("configfile: %s.repositories[%d].filter: %w", where, ri, err)
		}
	}
	return nil
}

func (in *Installation) resolve(where string, refs refPolicy) error {
	var err error
	if in.App != nil {
		in.App.clientID = in.App.ClientID
		if !in.App.ClientIDFrom.empty() {
			v, err := in.App.ClientIDFrom.resolve(refs)
			if err != nil {
				return fmt.Errorf("configfile: %s.app.clientIdFrom: %w", where, err)
			}
			in.App.clientID = v.Value()
		}
		if in.App.privateKey, err = in.App.PrivateKey.resolve(refs); err != nil {
			return fmt.Errorf("configfile: %s.app.privateKey: %w", where, err)
		}
		if in.App.webhookSecret, err = in.App.WebhookSecret.resolve(refs); err != nil {
			return fmt.Errorf("configfile: %s.app.webhookSecret: %w", where, err)
		}
	}
	for _, s := range []struct {
		name string
		ref  SecretRef
		dst  *Secret
	}{
		{"token", in.Token, &in.token},
		{"webhookSecret", in.WebhookSecret, &in.webhookSecret},
		{"gitToken", in.GitToken, &in.gitToken},
	} {
		if s.ref.empty() {
			continue
		}
		if *s.dst, err = s.ref.resolve(refs); err != nil {
			return fmt.Errorf("configfile: %s.%s: %w", where, s.name, err)
		}
	}
	return nil
}

// validate checks every invariant the rest of kritik relies on.
func (f *File) validate() error {
	if err := f.validateProviders(); err != nil {
		return err
	}
	if err := f.validateEgress(); err != nil {
		return err
	}
	if err := f.checkModels("defaults.models", f.Defaults.Models); err != nil {
		return err
	}
	if err := f.validateRetention(); err != nil {
		return err
	}
	if err := f.Web.validate(); err != nil {
		return err
	}
	return f.validateTenants()
}

// repositoryInstallation is the installation a repository entry binds to,
// or an error saying why it binds to none: the installation it names does
// not own it, no installation owns it, or several do and it names none.
func (t *Tenant) repositoryInstallation(r *Repository, where string) (*Installation, error) {
	owner, _, _ := strings.Cut(r.Name, "/")
	var owners []*Installation
	for i := range t.Installations {
		in := &t.Installations[i]
		if !strings.EqualFold(in.Account, owner) {
			continue
		}
		if in.Name == r.Installation {
			return in, nil
		}
		owners = append(owners, in)
	}
	switch {
	case r.Installation != "":
		return nil, fmt.Errorf("configfile: %s.installation %q is not an installation of tenant %q with account %q",
			where, r.Installation, t.Slug, owner)
	case len(owners) == 0:
		return nil, fmt.Errorf("configfile: %s.name %q: no installation in tenant %q has account %q", where, r.Name, t.Slug, owner)
	case len(owners) > 1:
		names := make([]string, len(owners))
		for i, in := range owners {
			names[i] = in.Name
		}
		return nil, fmt.Errorf("configfile: %s.name %q: installations %s of tenant %q all have account %q; set installation to one of them",
			where, r.Name, strings.Join(names, ", "), t.Slug, owner)
	}
	return owners[0], nil
}

func (f *File) validateRetention() error {
	if f.Retention.DisabledIndexGrace < 0 {
		return errors.New("configfile: retention.disabledIndexGrace must not be negative")
	}
	if f.Retention.Transcripts != 0 && f.Retention.Transcripts < minTranscripts {
		return fmt.Errorf("configfile: retention.transcripts must be at least %s", minTranscripts)
	}
	return nil
}

func (f *File) validateProviders() error {
	for name, p := range f.Providers {
		if !p.Type.Valid() {
			return fmt.Errorf("configfile: providers.%s.type must be %s, %s or %s, got %q",
				name, ProviderOpenRouter, ProviderOpenAI, ProviderAnthropic, p.Type)
		}
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("configfile: providers.%s.baseUrl %q must be an absolute URL", name, p.BaseURL)
			}
		}
		if p.apiKey.Value() == "" {
			return fmt.Errorf("configfile: providers.%s.apiKey resolved to an empty value", name)
		}
		for id, price := range p.Pricing {
			if price.Input < 0 || price.Output < 0 || price.CacheRead < 0 || price.CacheWrite < 0 {
				return fmt.Errorf("configfile: providers.%s.pricing.%s: prices must not be negative", name, id)
			}
		}
	}
	return nil
}

// validateRunnerDeadline checks that a tenant's runner.activeDeadlineSeconds is
// non-negative and, once converted to a job timeout, does not exceed River's cap.
func validateRunnerDeadline(where string, seconds int64) error {
	if seconds < 0 {
		return fmt.Errorf("configfile: %s.runner.activeDeadlineSeconds must not be negative", where)
	}
	deadline := time.Duration(seconds) * time.Second
	if deadline > jobtimeout.MaxRunnerDeadline {
		return fmt.Errorf("configfile: %s.runner.activeDeadlineSeconds must not exceed %d (%s),"+
			" or River's %s job timeout cap would cut the runner off early",
			where, int64(jobtimeout.MaxRunnerDeadline.Seconds()), jobtimeout.MaxRunnerDeadline, jobtimeout.MaxJobTimeout)
	}
	return nil
}

func (f *File) validateTenants() error {
	if err := checkLimits("defaults.limits", f.Defaults.Limits); err != nil {
		return err
	}
	if f.Defaults.Settle < 0 {
		return errors.New("configfile: defaults.settle must not be negative")
	}

	if len(f.Tenants) == 0 {
		return errors.New("configfile: tenants must list at least one tenant")
	}
	slugs := map[string]string{}
	installations := map[string]string{}
	for ti := range f.Tenants {
		t := &f.Tenants[ti]
		if err := f.validateTenant(t.where(ti), t, slugs, installations); err != nil {
			if t.Origin() == OriginDashboard {
				return &MergeError{Slug: t.Slug, Err: err}
			}
			return err
		}
	}
	return nil
}

// validateTenant checks one tenant. slugs and installations record the
// slugs and installation names already seen, so duplicates across tenants
// are caught whichever origin each has.
func (f *File) validateTenant(where string, t *Tenant, slugs, installations map[string]string) error {
	if !nameRe.MatchString(t.Slug) {
		return fmt.Errorf("configfile: %s.slug %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", where, t.Slug)
	}
	if prev, dup := slugs[t.Slug]; dup {
		return fmt.Errorf("configfile: %s.slug %q duplicates %s", where, t.Slug, prev)
	}
	slugs[t.Slug] = where
	if err := f.checkModels(where+".models", t.Models); err != nil {
		return err
	}
	if err := checkLimits(where+".limits", t.Limits); err != nil {
		return err
	}
	if t.Runner != nil {
		if err := validateRunnerDeadline(where, t.Runner.ActiveDeadlineSeconds); err != nil {
			return err
		}
	}
	if t.Settle < 0 {
		return fmt.Errorf("configfile: %s.settle must not be negative", where)
	}
	if len(t.Installations) == 0 {
		return fmt.Errorf("configfile: %s (%s) must list at least one installation", where, t.Slug)
	}
	for ii, in := range t.Installations {
		iwhere := fmt.Sprintf("%s.installations[%d]", where, ii)
		if !nameRe.MatchString(in.Name) {
			return fmt.Errorf("configfile: %s.name %q must be lowercase alphanumerics and hyphens, 1 to 63 characters", iwhere, in.Name)
		}
		if owner, dup := installations[in.Name]; dup {
			return fmt.Errorf("configfile: %s.name %q duplicates an installation in tenant %q; names are hook paths and must be unique",
				iwhere, in.Name, owner)
		}
		installations[in.Name] = t.Slug
		if in.Account == "" {
			return fmt.Errorf("configfile: %s.account is required", iwhere)
		}
		if err := in.validate(iwhere); err != nil {
			return err
		}
	}
	repos := map[string]int{}
	for ri, r := range t.Repositories {
		rwhere := fmt.Sprintf("%s.repositories[%d]", where, ri)
		if r.Name == "" || !strings.Contains(r.Name, "/") {
			return fmt.Errorf("configfile: %s.name must be \"owner/repo\", got %q", rwhere, r.Name)
		}
		in, err := t.repositoryInstallation(&r, rwhere)
		if err != nil {
			return err
		}
		key := in.Name + "\x00" + r.Name
		if prev, dup := repos[key]; dup {
			return fmt.Errorf("configfile: %s.name %q duplicates repositories[%d] of installation %q", rwhere, r.Name, prev, in.Name)
		}
		repos[key] = ri
		if r.Settle < 0 {
			return fmt.Errorf("configfile: %s.settle must not be negative", rwhere)
		}
		if err := r.validateReview(rwhere); err != nil {
			return err
		}
		for gi, g := range r.Ignore {
			if !doublestar.ValidatePattern(g) || strings.TrimSpace(g) == "" {
				return fmt.Errorf("configfile: %s.ignore[%d] %q is not a valid glob", rwhere, gi, g)
			}
		}
	}
	return nil
}

// validateReview checks the operator-only review keys of a repository.
func (r Repository) validateReview(where string) error {
	if r.Mode != "" && !r.Mode.Valid() {
		return fmt.Errorf("configfile: %s.mode must be %s or %s, got %q", where, ReviewSingle, ReviewAgentic, r.Mode)
	}
	for _, c := range []struct {
		name string
		v    *int
	}{
		{"agent.maxSteps", r.Agent.MaxSteps},
		{"agent.maxToolOutputBytes", r.Agent.MaxToolOutputBytes},
		{"incremental.maxDeltaFiles", r.Incremental.MaxDeltaFiles},
	} {
		if c.v != nil && *c.v <= 0 {
			return fmt.Errorf("configfile: %s.%s must be positive", where, c.name)
		}
	}
	if r.Agent.MaxTokens != nil && *r.Agent.MaxTokens <= 0 {
		return fmt.Errorf("configfile: %s.agent.maxTokens must be positive", where)
	}
	if r.Agent.Timeout != nil && *r.Agent.Timeout <= 0 {
		return fmt.Errorf("configfile: %s.agent.timeout must be positive", where)
	}
	if r.Agent.Timeout != nil && *r.Agent.Timeout > jobtimeout.MaxAgentTimeout {
		return fmt.Errorf("configfile: %s.agent.timeout must not exceed %s, or River's %s job timeout cap would cut the review short",
			where, jobtimeout.MaxAgentTimeout, jobtimeout.MaxJobTimeout)
	}
	// The job document carries whole seconds.
	if r.Agent.CommandTimeout != nil && *r.Agent.CommandTimeout < time.Second {
		return fmt.Errorf("configfile: %s.agent.commandTimeout must be at least 1s", where)
	}
	for i, c := range r.Agent.Commands {
		if !commandRe.MatchString(c) {
			return fmt.Errorf("configfile: %s.agent.commands[%d] %q must be a bare command name, not a path", where, i, c)
		}
		if slices.Contains(r.Agent.Commands[:i], c) {
			return fmt.Errorf("configfile: %s.agent.commands[%d] %q is listed twice", where, i, c)
		}
	}
	for i, p := range r.Review.Instructions {
		if err := checkRepoPath(p); err != nil {
			return fmt.Errorf("configfile: %s.review.instructions[%d]: %w", where, i, err)
		}
	}
	for _, t := range [][2]string{{"summary", r.Review.Templates.Summary}, {"inline", r.Review.Templates.Inline}} {
		if t[1] == "" {
			continue
		}
		if err := checkRepoPath(t[1]); err != nil {
			return fmt.Errorf("configfile: %s.review.templates.%s: %w", where, t[0], err)
		}
	}
	return nil
}

// checkRepoPath rejects a repository path that is empty, absolute or
// escapes the repository root.
func checkRepoPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path must not be empty")
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path %q must be relative", p)
	}
	if c := path.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("path %q escapes the repository", p)
	}
	return nil
}

func (in Installation) validate(where string) error {
	switch in.Forge {
	case ForgeGitHub:
		if in.App == nil {
			return fmt.Errorf("configfile: %s: a github installation needs an app", where)
		}
		if !in.Token.empty() || !in.WebhookSecret.empty() || !in.GitToken.empty() {
			return fmt.Errorf("configfile: %s: a github installation takes app credentials, not token, gitToken or webhookSecret", where)
		}
		if (in.App.ClientID == "") == in.App.ClientIDFrom.empty() {
			return fmt.Errorf("configfile: %s.app: set exactly one of clientId or clientIdFrom", where)
		}
		if in.App.clientID == "" {
			return fmt.Errorf("configfile: %s.app.clientIdFrom resolved to an empty value", where)
		}
		if in.App.privateKey.Value() == "" {
			return fmt.Errorf("configfile: %s.app.privateKey is required", where)
		}
		if in.App.webhookSecret.Value() == "" {
			return fmt.Errorf("configfile: %s.app.webhookSecret is required", where)
		}
	case ForgeGitLab, ForgeForgejo:
		if in.App != nil {
			return fmt.Errorf("configfile: %s: a %s installation takes a token, not an app", where, in.Forge)
		}
		if in.token.Value() == "" {
			return fmt.Errorf("configfile: %s.token is required", where)
		}
		if in.webhookSecret.Value() == "" {
			return fmt.Errorf("configfile: %s.webhookSecret is required", where)
		}
		if !in.GitToken.empty() && in.gitToken.Value() == "" {
			return fmt.Errorf("configfile: %s.gitToken resolved to an empty value", where)
		}
	default:
		return fmt.Errorf("configfile: %s.forge must be %s, %s or %s, got %q", where, ForgeGitHub, ForgeGitLab, ForgeForgejo, in.Forge)
	}
	return nil
}

func (f *File) checkModels(where string, m Models) error {
	for role, ref := range map[string]ModelRef{"review": m.Review, "fallback": m.Fallback} {
		if ref == "" {
			continue
		}
		p := ref.Provider()
		if p == "" || ref.Model() == "" {
			return fmt.Errorf("configfile: %s.%s must be \"<provider>/<model>\", got %q", where, role, ref)
		}
		if _, ok := f.Providers[p]; !ok {
			return fmt.Errorf("configfile: %s.%s references provider %q, which is not declared under providers", where, role, p)
		}
	}
	return nil
}

func checkLimits(where string, l Limits) error {
	if l.Concurrency < 0 || l.ReviewsPerDay < 0 || l.TokensPerMonth < 0 {
		return fmt.Errorf("configfile: %s: limits must not be negative", where)
	}
	return nil
}

// compileFilter compiles a CEL filter and smoke-tests it against a sample PR,
// so a filter that type-checks but fails at runtime (a field of the wrong
// type, a non-boolean result) is caught at load rather than on the first
// webhook. An empty filter compiles to nil, meaning "no restriction".
func compileFilter(expr string) (*prfilter.Program, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	prg, err := prfilter.Compile(expr)
	if err != nil {
		return nil, err
	}
	if _, err := prg.Eval(SamplePR()); err != nil {
		return nil, fmt.Errorf("smoke test against a sample pull request: %w", err)
	}
	return prg, nil
}

// SamplePR is the pull request every filter is evaluated against at load. It
// is also the documented shape of the `pr` variable: every key here is always
// present at runtime.
func SamplePR() map[string]any {
	return map[string]any{
		"number":    1,
		"title":     "feat: sample",
		"author":    "octocat",
		"state":     "open",
		"open":      true,
		"merged":    false,
		"draft":     false,
		"fork":      false,
		"headRef":   "feature",
		"headSha":   "0000000",
		"baseRef":   "main",
		"url":       "https://example.invalid/pull/1",
		"createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"labels":    []any{map[string]any{"name": "sample", "color": "ffffff"}},
		"body":      "sample body",
	}
}

func (r SecretRef) empty() bool { return r.Env == "" && r.File == "" && r.Sealed == "" }

// refPolicy is where a SecretRef may come from. The operator's file may read
// the environment and filesystem but carries no sealed values; a
// dashboard-managed tenant carries only sealed values, opened with open.
type refPolicy struct {
	dashboard bool
	open      Opener
}

var fileRefs = refPolicy{}

// resolve reads the referenced value. Exactly one of env, file or sealed
// must be set; an unset variable, an unreadable file or an unopenable sealed
// value is an error, never an empty value, so a typo cannot silently disable
// authentication.
func (r SecretRef) resolve(refs refPolicy) (Secret, error) {
	switch {
	case r.Sealed != "" && (r.Env != "" || r.File != ""):
		return Secret{}, errors.New("set exactly one of env, file or sealed")
	case r.Env != "" && r.File != "":
		return Secret{}, errors.New("set either env or file, not both")
	case r.Sealed != "" && !refs.dashboard:
		return Secret{}, errors.New("sealed values are only valid in dashboard-managed tenants")
	case refs.dashboard && (r.Env != "" || r.File != ""):
		return Secret{}, errors.New("dashboard-managed tenants take sealed values, not env or file references")
	case r.Sealed != "":
		if refs.open == nil {
			return Secret{}, errors.New("no key to open sealed values is configured")
		}
		b, err := refs.open.Open(r.Sealed)
		if err != nil {
			return Secret{}, fmt.Errorf("open sealed value: %w", err)
		}
		return Secret{value: strings.TrimRight(string(b), "\r\n")}, nil
	case r.Env != "":
		v, ok := os.LookupEnv(r.Env)
		if !ok {
			return Secret{}, fmt.Errorf("environment variable %s is not set", r.Env)
		}
		return Secret{value: strings.TrimRight(v, "\r\n")}, nil
	case r.File != "":
		b, err := os.ReadFile(r.File)
		if err != nil {
			return Secret{}, err
		}
		return Secret{value: strings.TrimRight(string(b), "\r\n")}, nil
	case refs.dashboard:
		return Secret{}, errors.New("reference must set sealed")
	default:
		return Secret{}, errors.New("reference must set env or file")
	}
}
