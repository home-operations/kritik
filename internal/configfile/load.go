package configfile

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/home-operations/kritika/internal/jobtimeout"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/prfilter"
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

// fileDoc is the configuration file's schema: sign-in and the whole
// configuration.
type fileDoc struct {
	Auth      Auth                `yaml:"auth,omitempty"`
	Apps      []Connection        `yaml:"apps,omitempty"`
	Providers map[string]Provider `yaml:"providers,omitempty"`
	Embedding *Embedding          `yaml:"embedding,omitempty"`
	Egress    Egress              `yaml:"egress,omitempty"`
	Defaults  Defaults            `yaml:"defaults,omitempty"`
	// Repositories are the owner/* and owner/name entries.
	Repositories map[string]Overrides  `yaml:"repositories,omitempty"`
	Accounts     map[string]accountDoc `yaml:"accounts,omitempty"`
}

// accountDoc is an entry of the file's accounts: what is an account's
// alone.
type accountDoc struct {
	Limits    LimitsSpec          `yaml:"limits,omitempty"`
	Providers map[string]Provider `yaml:"providers,omitempty"`
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

// overlayPrefixes start the environment variables that overlay the file;
// their values are part of the configuration, and so of its hash.
var overlayPrefixes = []string{authEnvPrefix, connectionEnvPrefix, providerEnvPrefix, defaultsEnvPrefix, embeddingEnvPrefix}

// configHash identifies a configuration: the file's bytes, the overlay
// variables that change what it says, and the values of the secrets it
// names, so two replicas with the same file but another environment do not
// compare equal.
func configHash(raw []byte, environ, secretEnv []string) string {
	h := sha256.New()
	h.Write(raw)
	var overlay []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(overlayPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			overlay = append(overlay, kv)
		}
	}
	slices.Sort(overlay)
	for _, kv := range overlay {
		h.Write([]byte("\x00" + kv))
	}
	for _, name := range secretEnv {
		h.Write([]byte("\x00" + name + "=" + os.Getenv(name)))
	}
	return hex.EncodeToString(h.Sum(nil))
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
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("configfile: parse: the file must hold one document")
		}
	}
	for i := range doc.Apps {
		doc.Apps[i].Forge = ForgeGitHub
	}
	environ := os.Environ()
	if err := doc.Auth.overlayEnv(environ); err != nil {
		return nil, err
	}
	envConnection, err := overlayConnectionEnv(&doc.Apps, environ)
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
	run, err := loadRun()
	if err != nil {
		return nil, err
	}
	accounts, err := accountsOf(doc.Accounts, doc.Repositories)
	if err != nil {
		return nil, err
	}
	f := &File{
		Auth: doc.Auth, Connections: doc.Apps, Providers: doc.Providers, Defaults: doc.Defaults, Egress: doc.Egress,
		Embedding: doc.Embedding, Run: run, envConnection: envConnection, envProvider: envProvider, envKeys: envKeys,
	}
	s := &secrets{}
	if err := f.Auth.resolve(s); err != nil {
		return nil, err
	}
	if err := f.resolve(accounts, s); err != nil {
		return nil, err
	}
	slices.Sort(s.env)
	f.secretEnv = s.env
	if err := f.Auth.validate(); err != nil {
		return nil, err
	}
	if err := f.validate(accounts); err != nil {
		return nil, err
	}
	f.Accounts, f.unserved = servedAccounts(f.Connections, accounts)
	f.hash = configHash(raw, environ, s.env)
	return f, nil
}

// resolve reads f's secrets and those of the account entries, and compiles
// their filters.
func (f *File) resolve(accounts []Account, s *secrets) error {
	for _, name := range slices.Sorted(maps.Keys(f.Providers)) {
		p := f.Providers[name]
		v, err := s.read(p.APIKey)
		if err != nil {
			return fmt.Errorf("configfile: providers.%s.apiKey: %w", name, err)
		}
		p.apiKey = v
		f.Providers[name] = p
	}
	f.Egress.credentials = make(map[string]Secret, len(f.Egress.Credentials))
	for _, host := range slices.Sorted(maps.Keys(f.Egress.Credentials)) {
		v, err := s.read(f.Egress.Credentials[host])
		if err != nil {
			return fmt.Errorf("configfile: egress.credentials.%s: %w", host, err)
		}
		f.Egress.credentials[strings.ToLower(host)] = v
	}
	if err := f.resolveEmbedding(); err != nil {
		return err
	}
	if err := f.Defaults.compile(); err != nil {
		return fmt.Errorf("configfile: defaults.filterExpr: %w", err)
	}
	for i := range f.Connections {
		if err := f.Connections[i].resolve(fmt.Sprintf("apps[%d]", i), s); err != nil {
			return err
		}
	}
	for i := range accounts {
		if err := accounts[i].resolve(s); err != nil {
			return err
		}
	}
	return nil
}

// resolveEmbedding gives the embedder its provider's endpoint and key: an
// openrouter or openai provider of the instance, since an account's own
// keys pay for its reviews, not the instance's index.
func (f *File) resolveEmbedding() error {
	e := f.Embedding
	if e == nil {
		return nil
	}
	name := e.Ref.Provider()
	if name == "" || e.Ref.Model() == "" {
		return fmt.Errorf("configfile: embedding.model must be \"<provider>/<model>\", got %q", e.Ref)
	}
	p, ok := f.Providers[name]
	if !ok {
		return fmt.Errorf("configfile: embedding.model references provider %q, which is not declared under providers", name)
	}
	switch p.Type {
	case ProviderOpenRouter:
		e.BaseURL = cmp.Or(p.BaseURL, model.OpenRouterBaseURL)
	case ProviderOpenAI:
		e.BaseURL = cmp.Or(p.BaseURL, model.OpenAIBaseURL)
	default:
		return fmt.Errorf("configfile: embedding.model names a %s provider; embeddings need an %s or %s one",
			p.Type, ProviderOpenRouter, ProviderOpenAI)
	}
	e.Model, e.apiKey = e.Ref.Model(), p.apiKey
	return nil
}

// accountsOf gathers each account's entries, in the order of their
// names: its accounts entry, and its owner/* and owner/name entries under
// repositories.
func accountsOf(entries map[string]accountDoc, repos map[string]Overrides) ([]Account, error) {
	byKey := map[string]*Account{}
	get := func(name string) *Account {
		key := AccountKey(ForgeGitHub, name)
		if a, ok := byKey[key]; ok {
			return a
		}
		a := &Account{Forge: ForgeGitHub, Name: name}
		byKey[key] = a
		return a
	}
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		where := "accounts." + name
		if err := checkAccountName(where, name); err != nil {
			return nil, err
		}
		a := get(name)
		if a.entry != "" {
			return nil, fmt.Errorf("configfile: %s duplicates %s", where, a.entry)
		}
		a.entry, a.Limits, a.Providers = where, entries[name].Limits, entries[name].Providers
	}
	seen := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(repos)) {
		where := "repositories." + key
		owner, name, ok := strings.Cut(key, "/")
		if !ok || checkAccountName(where, owner) != nil || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/ ") {
			return nil, fmt.Errorf("configfile: %s: a repository entry is keyed owner/* or owner/name", where)
		}
		if prev, dup := seen[strings.ToLower(key)]; dup {
			return nil, fmt.Errorf("configfile: %s duplicates %s", where, prev)
		}
		seen[strings.ToLower(key)] = where
		a := get(owner)
		if name == "*" {
			a.Overrides, a.pattern = repos[key], where
		} else {
			a.Repositories = append(a.Repositories, Repository{Name: name, Overrides: repos[key], where: where})
		}
	}
	out := make([]Account, 0, len(byKey))
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		out = append(out, *byKey[key])
	}
	return out, nil
}

// resolve reads the account's secret references and compiles its filters.
func (a *Account) resolve(s *secrets) error {
	if err := a.compile(); err != nil {
		return fmt.Errorf("configfile: %s.filterExpr: %w", a.pattern, err)
	}
	for _, name := range slices.Sorted(maps.Keys(a.Providers)) {
		p := a.Providers[name]
		v, err := s.read(p.APIKey)
		if err != nil {
			return fmt.Errorf("configfile: %s.providers.%s.apiKey: %w", a.entry, name, err)
		}
		p.apiKey = v
		a.Providers[name] = p
	}
	for ri := range a.Repositories {
		if err := a.Repositories[ri].compile(); err != nil {
			return fmt.Errorf("configfile: %s.filterExpr: %w", a.Repositories[ri].where, err)
		}
	}
	return nil
}

func (i *Connection) resolve(where string, s *secrets) error {
	var err error
	if i.App.clientID, err = i.App.ClientID.resolve(s); err != nil {
		return fmt.Errorf("configfile: %s.clientId: %w", where, err)
	}
	if i.App.privateKey, err = s.read(i.App.PrivateKey); err != nil {
		return fmt.Errorf("configfile: %s.privateKey: %w", where, err)
	}
	if i.App.webhookSecret, err = s.read(i.App.WebhookSecret); err != nil {
		return fmt.Errorf("configfile: %s.webhookSecret: %w", where, err)
	}
	return nil
}

// validate checks every invariant the rest of kritika relies on, over f and
// the account entries.
func (f *File) validate(accounts []Account) error {
	if err := f.validateProviders(); err != nil {
		return err
	}
	if err := f.validateEgress(); err != nil {
		return err
	}
	if err := f.validateEmbedding(); err != nil {
		return err
	}
	if err := validateConnections(f.Connections); err != nil {
		return err
	}
	return f.validateAccounts(accounts)
}

// validateTools checks the tool catalog: unique volume-safe names, an
// image each, a clean absolute path, and bare command names no two tools
// both provide.
func validateTools(tools []Tool) error {
	names, commands := map[string]bool{}, map[string]string{}
	for i, t := range tools {
		where := fmt.Sprintf("KRITIKA_RUNNER_TOOLS[%d]", i)
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

// validateEmbedding checks the embedder's dimension fits the index column
// and its bounds are not negative; its provider is checked with the rest.
func (f *File) validateEmbedding() error {
	e := f.Embedding
	if e == nil {
		return nil
	}
	if e.Dims <= 0 || e.Dims > MaxEmbedDims {
		return fmt.Errorf("configfile: embedding.dims must be between 1 and %d (the index's halfvec limit), got %d", MaxEmbedDims, e.Dims)
	}
	if e.MaxBatch < 0 || e.MaxBatchChars < 0 || e.MaxItemChars < 0 {
		return errors.New("configfile: embedding.maxBatch, maxBatchChars and maxItemChars must not be negative")
	}
	if e.SimilarFloor < 0 || e.SimilarFloor > 1 {
		return fmt.Errorf("configfile: embedding.similarFloor must be between 0 and 1 (a cosine similarity), got %g", e.SimilarFloor)
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

// validateAccounts checks the defaults and every account's entries,
// served or not, so an entry is judged when it is written rather than when
// a connection first serves it.
func (f *File) validateAccounts(accounts []Account) error {
	if err := checkLimits("defaults.limits", f.Defaults.Limits); err != nil {
		return err
	}
	if err := f.validateOverrides("defaults", nil, &f.Defaults.Overrides); err != nil {
		return err
	}
	for i := range accounts {
		if err := f.validateAccount(&accounts[i]); err != nil {
			return err
		}
	}
	return nil
}

// validateAccount checks an account's entries: its accounts entry, its
// owner/* entry, and its owner/name entries, which may not say where a
// repository starts.
func (f *File) validateAccount(a *Account) error {
	if a.entry != "" {
		if err := checkLimits(a.entry+".limits", a.Limits); err != nil {
			return err
		}
		if err := f.validateAccountProviders(a.entry, a); err != nil {
			return err
		}
	}
	if a.pattern != "" {
		if err := f.validateOverrides(a.pattern, a, &a.Overrides); err != nil {
			return err
		}
	}
	for _, r := range a.Repositories {
		if r.Enabled != nil {
			return fmt.Errorf("configfile: %s.enabled: turn a repository on or off in the dashboard", r.where)
		}
		if err := f.validateOverrides(r.where, a, &r.Overrides); err != nil {
			return err
		}
	}
	return nil
}

// compile compiles the filter the scope writes, if any; an empty one
// compiles to no restriction.
func (o *Overrides) compile() (err error) {
	if o.FilterExpr != nil {
		o.filter, err = compileFilter(*o.FilterExpr)
	}
	return err
}

// validateOverrides checks the settings one scope writes: its models name
// providers declared for account t (nil for the defaults), and its settle,
// maxAutoReviews, ignore globs, agent, incremental and review keys are in
// range.
func (f *File) validateOverrides(where string, t *Account, r *Overrides) error {
	if err := f.checkModels(where+".models", t, r.Models); err != nil {
		return err
	}
	if r.Settle != nil && *r.Settle < 0 {
		return fmt.Errorf("configfile: %s.settle must not be negative", where)
	}
	if r.MaxAutoReviews != nil && *r.MaxAutoReviews < 0 {
		return fmt.Errorf("configfile: %s.maxAutoReviews must not be negative", where)
	}
	for gi, g := range r.Ignore {
		if !ValidGlob(g) {
			return fmt.Errorf("configfile: %s.ignore[%d] %q is not a valid glob", where, gi, g)
		}
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
	return validateReview(where, &r.Review)
}

// validateReview checks the review keys one scope writes: their paths
// stay inside the repository, and the feedback is a level.
func validateReview(where string, r *ReviewSpec) error {
	if fb := r.Feedback; fb != nil && !ValidFeedback(*fb) {
		return fmt.Errorf("configfile: %s.feedback must be %s, got %q", where, FeedbackLevels, *fb)
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
	}{{"summaryTemplate", r.Comments.SummaryTemplate}, {"inlineTemplate", r.Comments.InlineTemplate}} {
		if t.path == nil || *t.path == "" {
			continue
		}
		if err := CheckRepoPath(*t.path); err != nil {
			return fmt.Errorf("configfile: %s.comments.%s: %w", where, t.name, err)
		}
	}
	return nil
}

// Check rejects a context file with no description, a path outside the
// repository, or a glob that is not valid.
func (c ContextFile) Check() error {
	if err := CheckRepoPath(c.Path); err != nil {
		return err
	}
	if strings.TrimSpace(c.Description) == "" {
		return errors.New("description is required")
	}
	for i, g := range c.Paths {
		if !ValidGlob(g) {
			return fmt.Errorf("paths[%d] %q is not a valid glob", i, g)
		}
	}
	return nil
}

// ValidGlob reports whether g is a doublestar glob a setting may take.
func ValidGlob(g string) bool { return strings.TrimSpace(g) != "" && doublestar.ValidatePattern(g) }

// ValidFeedback reports whether s is a feedback level.
func ValidFeedback(s string) bool {
	return s == FeedbackDetailed || s == FeedbackStandard || s == FeedbackMinimal
}

// FeedbackLevels lists the feedback levels for a message.
const FeedbackLevels = FeedbackDetailed + ", " + FeedbackStandard + " or " + FeedbackMinimal

// CheckRepoPath rejects a repository path that is empty, absolute or
// escapes the repository root: every path a configuration names is read
// in a checkout, so an unbounded one would read outside the repository.
func CheckRepoPath(p string) error {
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

func (i *Connection) validate(where string) error {
	switch i.Forge {
	case ForgeGitHub:
		if i.App.clientID == "" {
			return fmt.Errorf("configfile: %s.clientId is required", where)
		}
		if i.App.privateKey.Value() == "" {
			return fmt.Errorf("configfile: %s.privateKey is required", where)
		}
		if i.App.webhookSecret.Value() == "" {
			return fmt.Errorf("configfile: %s.webhookSecret is required", where)
		}
	default:
		return fmt.Errorf("configfile: %s.forge must be %s, got %q", where, ForgeGitHub, i.Forge)
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

func (r SecretRef) empty() bool { return r.Env == "" }

// secrets reads the secrets one Parse resolves, and records the variables
// they came from in env.
type secrets struct{ env []string }

// read resolves r. An unset variable is an error, never an empty value, so
// a typo cannot silently disable authentication.
func (s *secrets) read(r SecretRef) (Secret, error) {
	if r.Env == "" {
		return Secret{}, errors.New("reference must set env")
	}
	v, ok := os.LookupEnv(r.Env)
	if !ok {
		return Secret{}, fmt.Errorf("environment variable %s is not set", r.Env)
	}
	if !slices.Contains(s.env, r.Env) {
		s.env = append(s.env, r.Env)
	}
	return Secret{value: strings.TrimRight(v, "\r\n")}, nil
}
