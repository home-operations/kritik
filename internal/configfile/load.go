package configfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
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

// nameRe bounds connection and provider names to what is safe in a URL
// path segment, a Kubernetes label value and a log line.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// toolNameRe bounds a tool name to what fits a pod volume name after its
// "tool-" prefix: a DNS label of at most 63 characters.
var toolNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,56}[a-z0-9])?$`)

// commandRe is a binary name the run tool looks up on PATH: no path
// separator, so the allowlist cannot name a file in the checkout.
var commandRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// fileDoc is the configuration file's schema: sign-in and the connections
// an admin keeps in git (ADR-0014 §2.2), and the instance defaults the spec
// overrides (ADR-0015). Everything else is the spec's.
type fileDoc struct {
	Auth        Auth                `yaml:"auth,omitempty"`
	Connections []Connection        `yaml:"connections,omitempty"`
	Providers   map[string]Provider `yaml:"providers,omitempty"`
	Defaults    fileDefaults        `yaml:"defaults,omitempty"`
	Embedding   *Embedding          `yaml:"embedding,omitempty"`
}

// Load reads, decodes, resolves and validates the configuration file at
// name, overlaid with the environment. The file is optional: name "" loads
// the environment alone.
func Load(name string) (*File, error) {
	var raw []byte
	if name != "" {
		var err error
		if raw, err = os.ReadFile(name); err != nil {
			return nil, fmt.Errorf("configfile: %w", err)
		}
	}
	return Parse(raw)
}

// Parse is Load for bytes already in hand; nil or blank bytes are no file.
func Parse(raw []byte) (*File, error) {
	var doc fileDoc
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("configfile: parse: %w", err)
		}
	}
	environ := os.Environ()
	if err := doc.Auth.overlayEnv(environ); err != nil {
		return nil, err
	}
	envConnection, err := overlayConnectionEnv(&doc.Connections, environ)
	if err != nil {
		return nil, err
	}
	envProvider, err := overlayProviderEnv(&doc.Providers, environ)
	if err != nil {
		return nil, err
	}
	envKeys := map[string]bool{}
	if err := overlayDefaultsEnv(&doc.Defaults, environ, envKeys); err != nil {
		return nil, err
	}
	if err := overlayEmbeddingEnv(&doc.Embedding, environ, envKeys); err != nil {
		return nil, err
	}
	f := &File{
		Auth: doc.Auth, Connections: doc.Connections, Providers: doc.Providers, Embedding: doc.Embedding,
		envConnection: envConnection, envProvider: envProvider, envKeys: envKeys,
	}
	f.Defaults = doc.Defaults.defaults()
	if err := f.Auth.resolve(); err != nil {
		return nil, err
	}
	if err := f.resolveInstanceDefaults(); err != nil {
		return nil, err
	}
	for i := range f.Connections {
		f.Connections[i].origin = OriginFile
		if err := f.Connections[i].resolve(fmt.Sprintf("connections[%d]", i), fileRefs); err != nil {
			return nil, err
		}
	}
	if err := f.Auth.validate(); err != nil {
		return nil, err
	}
	if err := validateConnections(f.Connections); err != nil {
		return nil, err
	}
	// The default models are checked once merged, against every provider.
	if err := f.validateProviders(); err != nil {
		return nil, err
	}
	if err := f.validateEmbedding(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	f.hash = hex.EncodeToString(sum[:])
	return f, nil
}

// resolve reads the spec's sealed secrets with open and compiles its
// filters.
func (s *Spec) resolve(open Opener) error {
	refs := refPolicy{dashboard: true, open: open}
	for _, name := range slices.Sorted(maps.Keys(s.Providers)) {
		p := s.Providers[name]
		v, err := p.APIKey.resolve(refs)
		if err != nil {
			return fmt.Errorf("configfile: providers.%s.apiKey: %w", name, err)
		}
		p.apiKey = v
		s.Providers[name] = p
	}
	s.Egress.credentials = make(map[string]Secret, len(s.Egress.Credentials))
	for _, host := range slices.Sorted(maps.Keys(s.Egress.Credentials)) {
		v, err := s.Egress.Credentials[host].resolve(refs)
		if err != nil {
			return fmt.Errorf("configfile: egress.credentials.%s: %w", host, err)
		}
		s.Egress.credentials[strings.ToLower(host)] = v
	}
	if e := s.Embedding; e != nil {
		v, err := e.APIKey.resolve(refs)
		if err != nil {
			return fmt.Errorf("configfile: embedding.apiKey: %w", err)
		}
		e.apiKey = v
	}
	if err := s.Defaults.compile(); err != nil {
		return fmt.Errorf("configfile: defaults.filter: %w", err)
	}
	for i := range s.Connections {
		s.Connections[i].origin = OriginDashboard
		if err := s.Connections[i].resolve(fmt.Sprintf("connections[%d]", i), refs); err != nil {
			return err
		}
	}
	for i := range s.Accounts {
		if err := s.Accounts[i].resolve(fmt.Sprintf("accounts[%d]", i), refs); err != nil {
			return err
		}
	}
	return nil
}

// resolve reads the account's secret references under refs and compiles
// its filters; where prefixes every error.
func (a *Account) resolve(where string, refs refPolicy) error {
	if err := a.compile(); err != nil {
		return fmt.Errorf("configfile: %s.filter: %w", where, err)
	}
	for _, name := range slices.Sorted(maps.Keys(a.Providers)) {
		p := a.Providers[name]
		v, err := p.APIKey.resolve(refs)
		if err != nil {
			return fmt.Errorf("configfile: %s.providers.%s.apiKey: %w", where, name, err)
		}
		p.apiKey = v
		a.Providers[name] = p
	}
	for ri := range a.Repositories {
		if err := a.Repositories[ri].compile(); err != nil {
			return fmt.Errorf("configfile: %s.repositories[%d].filter: %w", where, ri, err)
		}
	}
	return nil
}

func (in *Connection) resolve(where string, refs refPolicy) error {
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
	return nil
}

// validate checks every invariant the rest of kritik relies on in the
// spec's settings, over the connections and accounts a merge runs.
func (f *File) validate(spec *Spec) error {
	if err := f.validateProviders(); err != nil {
		return err
	}
	if err := f.validateEgress(); err != nil {
		return err
	}
	if err := f.validateRetention(); err != nil {
		return err
	}
	if err := f.validateTuning(); err != nil {
		return err
	}
	if err := f.validateTools(); err != nil {
		return err
	}
	if err := f.validateEmbedding(); err != nil {
		return err
	}
	if err := validateConnections(spec.Connections); err != nil {
		return err
	}
	return f.validateAccounts(spec.Accounts)
}

// validateTools checks the tool catalog: unique volume-safe names, an
// image each, a clean absolute path, and bare command names no two tools
// both provide.
func (f *File) validateTools() error {
	names, commands := map[string]bool{}, map[string]string{}
	for i, t := range f.Tools {
		where := fmt.Sprintf("tools[%d]", i)
		if !toolNameRe.MatchString(t.Name) {
			return fmt.Errorf("configfile: %s.name %q must be lowercase alphanumerics and hyphens, 1 to 58 characters", where, t.Name)
		}
		if names[t.Name] {
			return fmt.Errorf("configfile: %s.name %q is listed twice", where, t.Name)
		}
		names[t.Name] = true
		if strings.TrimSpace(t.Image) == "" {
			return fmt.Errorf("configfile: %s.image is required", where)
		}
		if t.Path != "" && (!path.IsAbs(t.Path) || path.Clean(t.Path) != t.Path) {
			return fmt.Errorf("configfile: %s.path %q must be a clean absolute path inside the image", where, t.Path)
		}
		for _, c := range t.Provides() {
			if !commandRe.MatchString(c) {
				return fmt.Errorf("configfile: %s.commands %q must be a bare command name, not a path", where, c)
			}
			if other, dup := commands[c]; dup {
				return fmt.Errorf("configfile: %s provides %q, which tool %q already provides", where, c, other)
			}
			commands[c] = t.Name
		}
	}
	return nil
}

// validateEmbedding checks the embedder: an absolute endpoint, a key, a
// model, and a dimension the index column takes.
func (f *File) validateEmbedding() error {
	e := f.Embedding
	if e == nil {
		return nil
	}
	if u, err := url.Parse(e.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("configfile: embedding.baseUrl %q must be an absolute URL", e.BaseURL)
	}
	if e.apiKey.Value() == "" {
		return errors.New("configfile: embedding.apiKey resolved to an empty value")
	}
	if strings.TrimSpace(e.Model) == "" {
		return errors.New("configfile: embedding.model is required")
	}
	if e.Dims <= 0 || e.Dims > MaxEmbedDims {
		return fmt.Errorf("configfile: embedding.dims must be between 1 and %d (the index's halfvec limit), got %d", MaxEmbedDims, e.Dims)
	}
	if e.MaxBatch < 0 || e.MaxBatchChars < 0 || e.MaxItemChars < 0 {
		return errors.New("configfile: embedding.maxBatch, maxBatchChars and maxItemChars must not be negative")
	}
	return nil
}

// validateTuning checks defaults.runner, polling and indexing.
func (f *File) validateTuning() error {
	if r := f.Defaults.Runner; r != nil {
		if err := validateRunnerDeadline("defaults", r.ActiveDeadlineSeconds); err != nil {
			return err
		}
	}
	if (f.Polling.Interval != nil && *f.Polling.Interval < 0) || f.Polling.Lookback < 0 {
		return errors.New("configfile: polling.interval and polling.lookback must not be negative")
	}
	if f.Indexing.OnboardWindow < 0 {
		return errors.New("configfile: indexing.onboardWindow must not be negative")
	}
	return nil
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
	for _, name := range slices.Sorted(maps.Keys(f.Providers)) {
		if err := f.Providers[name].validate("providers." + name); err != nil {
			return err
		}
	}
	return nil
}

// validateRunnerDeadline checks that an account's runner.activeDeadlineSeconds is
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

// validateAccounts checks the defaults and every account entry of the
// spec, served or not, so an entry is judged when it is written rather than
// when a connection first serves it.
func (f *File) validateAccounts(entries []Account) error {
	if err := checkLimits("defaults.limits", f.Defaults.Limits); err != nil {
		return err
	}
	if err := f.validateOverrides("defaults", nil, &f.Defaults.Overrides); err != nil {
		return err
	}
	if err := checkWithinAllow("defaults", f.Settings(&Account{}, "")); err != nil {
		return err
	}
	seen := map[string]string{}
	for i := range entries {
		a := &entries[i]
		where := fmt.Sprintf("accounts[%d]", i)
		if prev, dup := seen[a.Key()]; dup {
			return fmt.Errorf("configfile: %s: account %s/%s duplicates %s", where, a.Forge, a.Name, prev)
		}
		seen[a.Key()] = where
		if err := f.validateAccount(where, a); err != nil {
			return err
		}
	}
	return nil
}

// validateAccount checks one account entry.
func (f *File) validateAccount(where string, a *Account) error {
	if a.Forge != ForgeGitHub {
		return fmt.Errorf("configfile: %s.forge must be %s, got %q", where, ForgeGitHub, a.Forge)
	}
	if err := checkAccountName(where+".name", a.Name); err != nil {
		return err
	}
	if err := checkLimits(where+".limits", a.Limits); err != nil {
		return err
	}
	if err := f.validateAccountProviders(where, a); err != nil {
		return err
	}
	if err := f.validateOverrides(where, a, &a.Overrides); err != nil {
		return err
	}
	if a.Runner != nil {
		if err := validateRunnerDeadline(where, a.Runner.ActiveDeadlineSeconds); err != nil {
			return err
		}
	}
	repos := map[string]int{}
	for ri, r := range a.Repositories {
		rwhere := fmt.Sprintf("%s.repositories[%d]", where, ri)
		if strings.TrimSpace(r.Name) == "" || strings.ContainsAny(r.Name, "/ ") {
			return fmt.Errorf("configfile: %s.name %q must be the repository's name without its owner", rwhere, r.Name)
		}
		key := strings.ToLower(r.Name)
		if prev, dup := repos[key]; dup {
			return fmt.Errorf("configfile: %s.name %q duplicates repositories[%d]", rwhere, r.Name, prev)
		}
		repos[key] = ri
		if err := f.validateOverrides(rwhere, a, &r.Overrides); err != nil {
			return err
		}
		if err := checkWithinAllow(rwhere, f.Settings(a, a.Name+"/"+r.Name)); err != nil {
			return err
		}
	}
	return checkWithinAllow(where, f.Settings(a, ""))
}

// compile compiles the filter the scope writes, if any; an empty one
// compiles to no restriction.
func (o *Overrides) compile() (err error) {
	if o.Filter != nil {
		o.filter, err = compileFilter(*o.Filter)
	}
	return err
}

// validateOverrides checks the settings one scope writes: its models name
// providers declared for account t (nil for the defaults), and its settle,
// ignore globs, mode, agent, incremental and review keys are in range.
func (f *File) validateOverrides(where string, t *Account, r *Overrides) error {
	if err := f.checkModels(where+".models", t, r.Models); err != nil {
		return err
	}
	if r.Settle != nil && *r.Settle < 0 {
		return fmt.Errorf("configfile: %s.settle must not be negative", where)
	}
	for gi, g := range r.Ignore {
		if !doublestar.ValidatePattern(g) || strings.TrimSpace(g) == "" {
			return fmt.Errorf("configfile: %s.ignore[%d] %q is not a valid glob", where, gi, g)
		}
	}
	if r.Mode != "" && !r.Mode.Valid() {
		return fmt.Errorf("configfile: %s.mode must be %s or %s, got %q", where, ReviewSingle, ReviewAgentic, r.Mode)
	}
	for _, c := range []struct {
		name string
		v    *int
	}{
		{keyMaxSteps, r.Agent.MaxSteps},
		{keyMaxToolOutputBytes, r.Agent.MaxToolOutputBytes},
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
	if err := validateReview(where+".review", &r.Review); err != nil {
		return err
	}
	return f.validateAllow(where+".allow", t, &r.Allow)
}

// validateReview checks the review block one scope writes: its paths stay
// inside the repository, and its severity floor and thoroughness are each
// one of the two.
func validateReview(where string, r *ReviewSpec) error {
	for i, p := range r.Instructions {
		if err := checkRepoPath(p); err != nil {
			return fmt.Errorf("configfile: %s.instructions[%d]: %w", where, i, err)
		}
	}
	if m := r.MinSeverity; m != nil && !ValidMinSeverity(*m) {
		return fmt.Errorf("configfile: %s.minSeverity must be %s or %s, got %q", where, SeverityNit, SeverityImportant, *m)
	}
	if th := r.Thoroughness; th != nil && !ValidThoroughness(*th) {
		return fmt.Errorf("configfile: %s.thoroughness must be %s or %s, got %q", where, ThoroughnessThorough, ThoroughnessFocused, *th)
	}
	for i, c := range r.Context {
		if err := c.Check(); err != nil {
			return fmt.Errorf("configfile: %s.context[%d]: %w", where, i, err)
		}
	}
	if err := CheckRules(r.Rules); err != nil {
		return fmt.Errorf("configfile: %s.%w", where, err)
	}
	for _, t := range []struct {
		name string
		path *string
	}{{"summary", r.Templates.Summary}, {"inline", r.Templates.Inline}} {
		if t.path == nil || *t.path == "" {
			continue
		}
		if err := checkRepoPath(*t.path); err != nil {
			return fmt.Errorf("configfile: %s.templates.%s: %w", where, t.name, err)
		}
	}
	return nil
}

// Check rejects a context file with no description, a path outside the
// repository, or a glob that is not valid.
func (c ContextFile) Check() error {
	if err := checkRepoPath(c.Path); err != nil {
		return err
	}
	if strings.TrimSpace(c.Description) == "" {
		return errors.New("description is required")
	}
	for i, g := range c.Paths {
		if strings.TrimSpace(g) == "" || !doublestar.ValidatePattern(g) {
			return fmt.Errorf("paths[%d] %q is not a valid glob", i, g)
		}
	}
	return nil
}

// ValidMinSeverity reports whether s is an inline severity floor; empty is
// none.
func ValidMinSeverity(s string) bool { return s == "" || s == SeverityNit || s == SeverityImportant }

// ValidThoroughness reports whether s is a review thoroughness.
func ValidThoroughness(s string) bool { return s == ThoroughnessThorough || s == ThoroughnessFocused }

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

func (in Connection) validate(where string) error {
	switch in.Forge {
	case ForgeGitHub:
		if in.App == nil {
			return fmt.Errorf("configfile: %s: a github connection needs an app", where)
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
	default:
		return fmt.Errorf("configfile: %s.forge must be %s, got %q", where, ForgeGitHub, in.Forge)
	}
	return nil
}

func (f *File) checkModels(where string, t *Account, m ModelsSpec) error {
	for _, r := range []struct {
		role string
		ref  *ModelRef
	}{{"review", m.Review}, {"fallback", m.Fallback}} {
		if r.ref == nil || *r.ref == "" {
			continue
		}
		if err := f.checkModelRef(where+"."+r.role, t, *r.ref); err != nil {
			return err
		}
	}
	return nil
}

// checkModelRef rejects a model reference that is not
// "<provider>/<model>" of a provider declared for account t or the
// instance.
func (f *File) checkModelRef(where string, t *Account, ref ModelRef) error {
	p := ref.Provider()
	if p == "" || ref.Model() == "" {
		return fmt.Errorf("configfile: %s must be \"<provider>/<model>\", got %q", where, ref)
	}
	if _, ok := f.Provider(t, p); !ok {
		return fmt.Errorf("configfile: %s references provider %q, which is not declared under providers", where, p)
	}
	return nil
}

// validateAllow checks one scope's bounds name what a repository could
// choose: review modes, models of declared providers, bare command names,
// positive limits and a settle time that is not negative.
func (f *File) validateAllow(where string, t *Account, a *Allow) error {
	for i, m := range a.Modes {
		if !m.Valid() {
			return fmt.Errorf("configfile: %s.modes[%d] must be %s or %s, got %q", where, i, ReviewSingle, ReviewAgentic, m)
		}
	}
	for i, ref := range a.Models {
		if err := f.checkModelRef(fmt.Sprintf("%s.models[%d]", where, i), t, ref); err != nil {
			return err
		}
	}
	for i, c := range a.Commands {
		if !commandRe.MatchString(c) {
			return fmt.Errorf("configfile: %s.commands[%d] %q must be a bare command name, not a path", where, i, c)
		}
	}
	ag := a.Agent
	if (ag.MaxSteps != nil && *ag.MaxSteps <= 0) || (ag.MaxToolOutputBytes != nil && *ag.MaxToolOutputBytes <= 0) ||
		(ag.MaxTokens != nil && *ag.MaxTokens <= 0) || (ag.Timeout != nil && *ag.Timeout <= 0) {
		return fmt.Errorf("configfile: %s.agent bounds must be positive", where)
	}
	if ag.Timeout != nil && *ag.Timeout > jobtimeout.MaxAgentTimeout {
		return fmt.Errorf("configfile: %s.agent.timeout must not exceed %s", where, jobtimeout.MaxAgentTimeout)
	}
	if a.Settle != nil && *a.Settle < 0 {
		return fmt.Errorf("configfile: %s.settle must not be negative", where)
	}
	return nil
}

// checkWithinAllow rejects resolved settings whose own values lie outside
// the bounds they give the repository: the repository would be refused
// the admin's own choice.
func checkWithinAllow(where string, s Settings) error {
	a := s.Allow
	if a.Modes != nil && !slices.Contains(a.Modes, s.Mode) {
		return fmt.Errorf("configfile: %s: mode %s is outside allow.modes", where, s.Mode)
	}
	for _, m := range []struct {
		role string
		ref  ModelRef
	}{{"review", s.Models.Review}, {"fallback", s.Models.Fallback}} {
		if a.Models != nil && m.ref != "" && !slices.Contains(a.Models, m.ref) {
			return fmt.Errorf("configfile: %s: models.%s %q is outside allow.models", where, m.role, m.ref)
		}
	}
	for _, c := range s.Agent.Commands {
		if a.Commands != nil && !slices.Contains(a.Commands, c) {
			return fmt.Errorf("configfile: %s: agent.commands %q is outside allow.commands", where, c)
		}
	}
	var over string
	switch ag, bound := s.Agent, a.Agent; {
	case bound.MaxSteps != nil && ag.MaxSteps > *bound.MaxSteps:
		over = keyMaxSteps
	case bound.MaxToolOutputBytes != nil && ag.MaxToolOutputBytes > *bound.MaxToolOutputBytes:
		over = keyMaxToolOutputBytes
	case bound.MaxTokens != nil && ag.MaxTokens > *bound.MaxTokens:
		over = keyMaxTokens
	case bound.Timeout != nil && ag.Timeout > *bound.Timeout:
		over = keyTimeout
	case a.Settle != nil && s.Settle > *a.Settle:
		over = keySettle
	}
	if over != "" {
		return fmt.Errorf("configfile: %s: %s is above allow.%s; set it at or below the bound", where, over, over)
	}
	return nil
}

func checkLimits(where string, l LimitsSpec) error {
	if (l.ReviewsPerDay != nil && *l.ReviewsPerDay < 0) || (l.TokensPerMonth != nil && *l.TokensPerMonth < 0) {
		return fmt.Errorf("configfile: %s: limits must not be negative", where)
	}
	if l.Concurrency != nil && *l.Concurrency <= 0 {
		return fmt.Errorf("configfile: %s.concurrency must be positive", where)
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
		"event":     "opened",
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

// refPolicy is where a SecretRef may come from. The configuration file may
// read the environment and filesystem but carries no sealed values; the
// spec carries only sealed values, opened with open.
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
		return Secret{}, errors.New("sealed values are only valid in the dashboard's configuration")
	case refs.dashboard && (r.Env != "" || r.File != ""):
		return Secret{}, errors.New("the dashboard's configuration takes sealed values, not env or file references")
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
