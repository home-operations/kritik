// Package configfile loads the declarative configuration file: the model
// providers, defaults, tenants, installations and repositories kritik
// manages. Process configuration (addresses, database, log level) is
// environment variables and lives in internal/config.
//
// The file is the operator's source of truth and is applied atomically: the
// whole document is decoded with unknown keys rejected, every secret
// reference resolved, every filter compiled and smoke-tested, and every
// invariant checked before any of it is returned. A bad file is an error and
// the caller keeps the last good state.
package configfile

import (
	"slices"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/prfilter"
)

// ProviderType selects the model adapter a provider uses.
type ProviderType = model.ProviderType

// Provider types kritik implements. Each accepts a baseUrl, so any gateway
// compatible with the OpenAI or Anthropic API is a provider.
const (
	ProviderOpenRouter = model.ProviderOpenRouter
	ProviderOpenAI     = model.ProviderOpenAI
	ProviderAnthropic  = model.ProviderAnthropic
)

// Forge identifies which forge an installation talks to.
type Forge string

// Forges kritik supports.
const (
	ForgeGitHub  Forge = "github"
	ForgeGitLab  Forge = "gitlab"
	ForgeForgejo Forge = "forgejo"
)

// SecretRef points at where a secret value lives. Exactly one of Env or File
// is set. Values are resolved at load and never written back to disk or the
// database.
type SecretRef struct {
	Env  string `yaml:"env,omitempty"`
	File string `yaml:"file,omitempty"`
}

// Secret is a resolved secret value. Its String method redacts, so a Secret
// can be logged or formatted without leaking; call Value to use it.
type Secret struct {
	value string
}

// Value returns the secret material.
func (s Secret) Value() string { return s.value }

// String redacts.
func (s Secret) String() string {
	if s.value == "" {
		return ""
	}
	return "<redacted>"
}

// GoString redacts in %#v output too.
func (s Secret) GoString() string { return s.String() }

// Provider is a model endpoint. Keys are references, never values, so the
// file can live in git.
type Provider struct {
	Type ProviderType `yaml:"type"`
	// BaseURL overrides the type's default endpoint.
	BaseURL string    `yaml:"baseUrl,omitempty"`
	APIKey  SecretRef `yaml:"apiKey"`
	// Pricing, keyed by model id, computes the cost of calls the provider
	// does not report a cost for; without it such calls cost zero while
	// their tokens still count against limits.
	Pricing model.Pricing `yaml:"pricing,omitempty"`

	apiKey Secret
}

// APIKeyValue returns the resolved API key.
func (p Provider) APIKeyValue() Secret { return p.apiKey }

// ModelRef names a model as "<provider>/<model>", where provider is a key of
// the file's providers map and model is whatever the provider accepts.
type ModelRef string

// Provider returns the provider half of the reference, or "" when the
// reference has no slash.
func (m ModelRef) Provider() string {
	p, _, ok := strings.Cut(string(m), "/")
	if !ok {
		return ""
	}
	return p
}

// Model returns the model half of the reference, or "" when the reference
// has no slash.
func (m ModelRef) Model() string {
	_, id, ok := strings.Cut(string(m), "/")
	if !ok {
		return ""
	}
	return id
}

// Models are the per-tenant completion roles. Empty means "inherit from the
// level above"; a role still empty after resolution means the feature is
// off. The embedding model is not here: it is deployment-wide and lives in
// the process environment, because changing it reindexes every repository.
type Models struct {
	Review   ModelRef `yaml:"review,omitempty"`
	Fallback ModelRef `yaml:"fallback,omitempty"`
}

// Limits bound what a tenant may consume. Zero means "inherit"; a limit
// still zero after resolution is unset, except Concurrency, which falls back
// to DefaultConcurrency.
type Limits struct {
	// Concurrency is the number of advisory-lock slots per tenant and model:
	// how many model calls may run at once.
	Concurrency int `yaml:"concurrency,omitempty"`
	// ReviewsPerDay caps review passes per tenant per calendar day.
	ReviewsPerDay int `yaml:"reviewsPerDay,omitempty"`
	// TokensPerMonth caps input plus output tokens per tenant per calendar
	// month.
	TokensPerMonth int64 `yaml:"tokensPerMonth,omitempty"`
}

// DefaultConcurrency applies when no level of the file sets one.
const DefaultConcurrency = 2

// Runner overrides for the Kubernetes Job a tenant's index and review pods
// run as. Kept as loose maps for resources because the values are copied
// verbatim into the pod spec and the Kubernetes types are not a dependency
// of this package.
type Runner struct {
	Resources             map[string]any `yaml:"resources,omitempty"`
	ActiveDeadlineSeconds int64          `yaml:"activeDeadlineSeconds,omitempty"`
}

// Defaults apply to every tenant unless overridden.
type Defaults struct {
	Models Models `yaml:"models,omitempty"`
	Filter string `yaml:"filter,omitempty"`
	Forks  *bool  `yaml:"forks,omitempty"`
	Limits Limits `yaml:"limits,omitempty"`
	// Settle delays a review job for a new head, so a burst of pushes
	// collapses onto the last one before anything is spent.
	Settle time.Duration `yaml:"settle,omitempty"`
}

// Retention controls what is deleted and when. Reviews, findings and usage
// are kept indefinitely; only bulky, reproducible data expires.
type Retention struct {
	// DisabledIndexGrace is how long a disabled repository's vectors are
	// kept before deletion, so re-enabling within the window reuses the
	// index instead of rebuilding it.
	DisabledIndexGrace time.Duration `yaml:"disabledIndexGrace,omitempty"`
}

// DefaultDisabledIndexGrace applies when the file sets no retention.
const DefaultDisabledIndexGrace = 30 * 24 * time.Hour

// DefaultIgnore is always skipped by chunking and the caller search, on top
// of whatever the operator's file and the in-repo file add. Vendored and
// generated trees otherwise dominate both.
var DefaultIgnore = []string{
	"vendor/**",
	"node_modules/**",
	"**/*.lock",
	"**/package-lock.json",
	"**/go.sum",
}

// GitHubApp is a GitHub App credential owned by an installation. The client
// id is not secret, but operators often keep it next to the key, so it may
// be given inline or by reference; exactly one of the two.
type GitHubApp struct {
	ClientID      string    `yaml:"clientId,omitempty"`
	ClientIDFrom  SecretRef `yaml:"clientIdFrom,omitempty"`
	PrivateKey    SecretRef `yaml:"privateKey"`
	WebhookSecret SecretRef `yaml:"webhookSecret"`

	clientID      string
	privateKey    Secret
	webhookSecret Secret
}

// ClientIDValue returns the client id, inline or resolved.
func (a GitHubApp) ClientIDValue() string { return a.clientID }

// PrivateKeyValue returns the resolved private key PEM.
func (a GitHubApp) PrivateKeyValue() Secret { return a.privateKey }

// WebhookSecretValue returns the resolved webhook secret.
func (a GitHubApp) WebhookSecretValue() Secret { return a.webhookSecret }

// Installation is one bot on one forge account. Its name is the hook path,
// /hooks/{name}, and must be unique across the whole file.
type Installation struct {
	Name    string `yaml:"name"`
	Forge   Forge  `yaml:"forge"`
	Host    string `yaml:"host,omitempty"`
	Account string `yaml:"account"`

	// App is set for GitHub installations.
	App *GitHubApp `yaml:"app,omitempty"`
	// Token and WebhookSecret are set for GitLab and Forgejo installations.
	Token         SecretRef `yaml:"token,omitempty"`
	WebhookSecret SecretRef `yaml:"webhookSecret,omitempty"`
	// GitToken, optional for GitLab and Forgejo, is the token runner pods
	// fetch with in place of Token. Token can write to the forge and would
	// otherwise reach the pod that reads untrusted content, so a read-only
	// token belongs here.
	GitToken SecretRef `yaml:"gitToken,omitempty"`

	token         Secret
	webhookSecret Secret
	gitToken      Secret
}

// TokenValue returns the resolved bot token for GitLab and Forgejo.
func (i Installation) TokenValue() Secret { return i.token }

// GitTokenValue returns the resolved fetch token for GitLab and Forgejo,
// empty when none is configured.
func (i Installation) GitTokenValue() Secret { return i.gitToken }

// WebhookSecretValue returns the resolved webhook secret for any forge.
func (i Installation) WebhookSecretValue() Secret {
	if i.App != nil {
		return i.App.webhookSecret
	}
	return i.webhookSecret
}

// Repository carries per-repository overrides. Everything an installation
// grants access to is watched whether or not it is listed here.
type Repository struct {
	Name     string        `yaml:"name"`
	Enabled  *bool         `yaml:"enabled,omitempty"`
	Filter   string        `yaml:"filter,omitempty"`
	Konflate string        `yaml:"konflate,omitempty"`
	Ignore   []string      `yaml:"ignore,omitempty"`
	Settle   time.Duration `yaml:"settle,omitempty"`
	// Mode, Agent and Incremental are operator-only: the in-repo file
	// cannot change how much a review may spend.
	Mode        ReviewMode  `yaml:"mode,omitempty"`
	Agent       Agent       `yaml:"agent,omitempty"`
	Incremental Incremental `yaml:"incremental,omitempty"`
	// Review holds defaults the in-repo file may override.
	Review Review `yaml:"review,omitempty"`

	filter *prfilter.Program
}

// ReviewMode is how a review is carried out.
type ReviewMode string

// Review modes. An unset mode resolves to single.
const (
	ReviewSingle  ReviewMode = "single"
	ReviewAgentic ReviewMode = "agentic"
)

// Valid reports whether m is a review mode.
func (m ReviewMode) Valid() bool { return m == ReviewSingle || m == ReviewAgentic }

func (m ReviewMode) String() string { return string(m) }

// Agent bounds an agentic review. A field left unset takes its default from
// DefaultAgent; one that is set must be positive.
type Agent struct {
	MaxSteps           *int `yaml:"maxSteps,omitempty"`
	MaxToolOutputBytes *int `yaml:"maxToolOutputBytes,omitempty"`
	// MaxTokens bounds the prompt plus output tokens one agentic review
	// may spend across all its steps.
	MaxTokens *int64         `yaml:"maxTokens,omitempty"`
	Timeout   *time.Duration `yaml:"timeout,omitempty"`
	// Commands name the binaries the agent's run tool may execute (ADR-0008),
	// such as curl, fd and rg. The tool is offered only for names the
	// runner image has on its PATH, so the distroless image offers none.
	Commands []string `yaml:"commands,omitempty"`
	// CommandTimeout bounds one command the run tool executes.
	CommandTimeout *time.Duration `yaml:"commandTimeout,omitempty"`
}

// AgentSettings are the resolved agent bounds.
type AgentSettings struct {
	MaxSteps           int
	MaxToolOutputBytes int
	MaxTokens          int64
	Timeout            time.Duration
	Commands           []string
	CommandTimeout     time.Duration
}

// DefaultAgent applies to every agent bound a repository leaves unset. Its
// MaxTokens is the agent loop's own default budget. No command is allowed
// by default: a repository is opted into the run tool.
var DefaultAgent = AgentSettings{
	MaxSteps: 60, MaxToolOutputBytes: 32 << 10, MaxTokens: 4_000_000, Timeout: 20 * time.Minute, CommandTimeout: 30 * time.Second,
}

// Incremental tunes incremental re-review.
type Incremental struct {
	// MaxDeltaFiles is how many files may change since the last review
	// before a re-review covers the whole pull request again.
	MaxDeltaFiles *int `yaml:"maxDeltaFiles,omitempty"`
}

// IncrementalSettings are the resolved incremental settings.
type IncrementalSettings struct {
	MaxDeltaFiles int
}

// DefaultMaxDeltaFiles applies when a repository sets no maxDeltaFiles.
const DefaultMaxDeltaFiles = 25

// ReviewTemplates name repository files, read from the merge base, that
// replace the built-in comment templates.
type ReviewTemplates struct {
	Summary string `yaml:"summary,omitempty"`
	Inline  string `yaml:"inline,omitempty"`
}

// Review is the operator's presentation and strictness defaults for a
// repository. Paths name files in the repository's merge-base tree.
type Review struct {
	Instructions        []string        `yaml:"instructions,omitempty"`
	RequireSuggestedFix bool            `yaml:"requireSuggestedFix,omitempty"`
	Templates           ReviewTemplates `yaml:"templates,omitempty"`
}

// Referenced lists the repository paths the block names: instructions
// first, then the summary and inline templates, deduplicated.
func (r Review) Referenced() []string {
	var out []string
	for _, p := range append(append([]string(nil), r.Instructions...), r.Templates.Summary, r.Templates.Inline) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Tenant is a forge account and the unit of isolation.
type Tenant struct {
	Slug          string         `yaml:"slug"`
	Runner        *Runner        `yaml:"runner,omitempty"`
	Installations []Installation `yaml:"installations"`
	Models        Models         `yaml:"models,omitempty"`
	Filter        string         `yaml:"filter,omitempty"`
	Forks         *bool          `yaml:"forks,omitempty"`
	Limits        Limits         `yaml:"limits,omitempty"`
	Repositories  []Repository   `yaml:"repositories,omitempty"`
	Settle        time.Duration  `yaml:"settle,omitempty"`

	filter *prfilter.Program
}

// Egress is what runner pods may reach through the worker's gateway beyond
// the forges and model endpoints the file itself names, which are always
// allowed. Hosts are exact, or a suffix with a leading "*."; the gateway
// tunnels TLS to port 443 only. A credential is the token the gateway adds,
// as a bearer, to a plain http:// request a runner makes to that host, so
// the runner can use an API at a token's rate limit without holding it.
type Egress struct {
	AllowHosts  []string             `yaml:"allowHosts,omitempty"`
	Credentials map[string]SecretRef `yaml:"credentials,omitempty"`

	credentials map[string]Secret
}

// File is the whole configuration document.
type File struct {
	Providers map[string]Provider `yaml:"providers,omitempty"`
	Defaults  Defaults            `yaml:"defaults,omitempty"`
	Retention Retention           `yaml:"retention,omitempty"`
	Egress    Egress              `yaml:"egress,omitempty"`
	Tenants   []Tenant            `yaml:"tenants"`

	defaultFilter *prfilter.Program
	hash          string
}

// Hash is the hex SHA-256 of the file's bytes as parsed. The leader records
// it in the store after applying the file, and followers compare it with
// their own copy to report drift.
func (f *File) Hash() string { return f.hash }

// Settings are the effective settings for one repository after defaults,
// tenant and repository layers are merged.
type Settings struct {
	Enabled  bool
	Models   Models
	Filter   *prfilter.Program
	Forks    bool
	Limits   Limits
	Konflate string
	// Ignore is DefaultIgnore plus the repository's own globs. The in-repo
	// file's globs are unioned in by the caller that has the checkout.
	Ignore []string
	// Settle delays a review job for a new head; zero means immediate.
	Settle      time.Duration
	Mode        ReviewMode
	Agent       AgentSettings
	Incremental IncrementalSettings
	Review      Review
}
