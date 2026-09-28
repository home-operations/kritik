// Package configfile loads the declarative configuration file: the model
// providers, defaults, tenants, connections and repositories kritik
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

// Forge identifies which forge a connection talks to.
type Forge string

// ForgeGitHub is github.com, the one forge kritik supports. Forge stays a
// type, and the code that switches on it keeps its switch, so another forge
// can be added back (ADR-0014).
const ForgeGitHub Forge = "github"

// SecretRef points at where a secret value lives. Exactly one of Env, File
// or Sealed is set. Values are resolved at load and never written back to
// disk or the database. Sealed is ciphertext only a dashboard-managed tenant
// may carry; Env and File would read the server's own environment and
// filesystem, so only the operator's file may use them.
type SecretRef struct {
	Env    string `yaml:"env,omitempty"`
	File   string `yaml:"file,omitempty"`
	Sealed string `yaml:"sealed,omitempty"`
}

// Opener decrypts a sealed secret value.
type Opener interface {
	Open(sealed string) ([]byte, error)
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
// the tenant's or the file's providers map and model is whatever the
// provider accepts.
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

// Models are the resolved completion roles; a role empty after resolution
// means the feature is off. The embedding model is not here: it is
// deployment-wide and lives in the process environment, because changing it
// reindexes every repository.
type Models struct {
	Review   ModelRef
	Fallback ModelRef
}

// ModelsSpec sets the completion roles at one scope. A role written here,
// even empty, replaces the broader scope's; one left out inherits it.
type ModelsSpec struct {
	Review   *ModelRef `yaml:"review,omitempty"`
	Fallback *ModelRef `yaml:"fallback,omitempty"`
}

// Limits bound what a tenant may consume, as resolved. A cap of zero is no
// cap; Concurrency is never zero once resolved.
type Limits struct {
	// Concurrency is the number of advisory-lock slots per tenant and model:
	// how many model calls may run at once.
	Concurrency int
	// ReviewsPerDay caps review passes per tenant per calendar day.
	ReviewsPerDay int
	// TokensPerMonth caps input plus output tokens per tenant per calendar
	// month.
	TokensPerMonth int64
}

// LimitsSpec sets limits at one scope. A limit written here replaces the
// broader scope's, so an explicit 0 lifts a cap the defaults set; one left
// out inherits it. Concurrency, when written, must be positive; unset
// everywhere it is DefaultConcurrency.
type LimitsSpec struct {
	Concurrency    *int   `yaml:"concurrency,omitempty"`
	ReviewsPerDay  *int   `yaml:"reviewsPerDay,omitempty"`
	TokensPerMonth *int64 `yaml:"tokensPerMonth,omitempty"`
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

// DefaultRunnerDeadline bounds a runner Job when neither the tenant nor
// defaults.runner sets activeDeadlineSeconds.
const DefaultRunnerDeadline = 15 * time.Minute

// Defaults apply to every tenant unless overridden.
type Defaults struct {
	// Runner is every tenant's runner block unless the tenant sets its own
	// deadline or resources.
	Runner    *Runner `yaml:"runner,omitempty"`
	Overrides `yaml:",inline"`
	Limits    LimitsSpec `yaml:"limits,omitempty"`
}

// Overrides are the repository settings every operator scope may set: the
// defaults, a tenant and a repository entry. A field a narrower scope
// writes replaces the broader scope's, even when it is empty or zero; a
// field it leaves out inherits (ADR-0010 §2.4). Ignore globs are unioned
// instead.
type Overrides struct {
	Models ModelsSpec `yaml:"models,omitempty"`
	Filter *string    `yaml:"filter,omitempty"`
	Forks  *bool      `yaml:"forks,omitempty"`
	Ignore []string   `yaml:"ignore,omitempty"`
	// Settle delays a review job for a new head, so a burst of pushes
	// collapses onto the last one before anything is spent.
	Settle      *time.Duration `yaml:"settle,omitempty"`
	Mode        ReviewMode     `yaml:"mode,omitempty"`
	Agent       Agent          `yaml:"agent,omitempty"`
	Incremental Incremental    `yaml:"incremental,omitempty"`
	Review      ReviewSpec     `yaml:"review,omitempty"`
	// Allow bounds what the repository's own .kritik.yaml may choose.
	Allow Allow `yaml:"allow,omitempty"`

	filter *prfilter.Program
}

// Allow bounds what a repository's .kritik.yaml may choose (ADR-0010
// §2.5), bound by bound: one written at a narrower scope replaces the
// broader scope's, even when empty. A bound written nowhere leaves a
// repository only the operator's own mode, model and commands, and limits
// and a settle time at or below the operator's own.
type Allow struct {
	Modes    []ReviewMode `yaml:"modes,omitempty"`
	Models   []ModelRef   `yaml:"models,omitempty"`
	Commands []string     `yaml:"commands,omitempty"`
	// Agent caps each agent limit a repository may set.
	Agent  AllowAgent     `yaml:"agent,omitempty"`
	Settle *time.Duration `yaml:"settle,omitempty"`
}

// AllowAgent caps the agent limits a repository may set.
type AllowAgent struct {
	MaxSteps           *int           `yaml:"maxSteps,omitempty"`
	MaxToolOutputBytes *int           `yaml:"maxToolOutputBytes,omitempty"`
	MaxTokens          *int64         `yaml:"maxTokens,omitempty"`
	Timeout            *time.Duration `yaml:"timeout,omitempty"`
}

// Polling is the leader's backstop for missed webhooks: it lists each
// connection's open pull requests every Interval.
type Polling struct {
	// Interval is how often to poll; an explicit 0 turns polling off, and
	// unset is DefaultPollInterval.
	Interval *time.Duration `yaml:"interval,omitempty"`
	// Lookback bounds how far back a first or long-idle poll looks, so a
	// long outage does not list every open pull request's history at once.
	Lookback time.Duration `yaml:"lookback,omitempty"`
}

// Poll defaults, when the file sets none.
const (
	DefaultPollInterval = 10 * time.Minute
	DefaultPollLookback = 24 * time.Hour
)

// Tool is a command-line tool a runner pod mounts from an image, read-only,
// for the agent's run tool (ADR-0011). The runner image's own tools (curl,
// fd and rg in the -tools image) need no entry.
type Tool struct {
	// Name identifies the tool; it names the pod volume.
	Name string `yaml:"name"`
	// Image is the image the tool comes from; pin it by digest.
	Image string `yaml:"image"`
	// Path is the directory inside the image that holds the binaries; it
	// goes first on the runner's PATH. Default "/". The binaries must be
	// statically linked: the default runner image has no libc.
	Path string `yaml:"path,omitempty"`
	// Commands are the binaries the tool provides, the names agent.commands
	// allows; default the tool's name.
	Commands []string `yaml:"commands,omitempty"`
}

// Provides lists the commands the tool puts on the runner's PATH.
func (t Tool) Provides() []string {
	if len(t.Commands) == 0 {
		return []string{t.Name}
	}
	return t.Commands
}

// Indexing tunes how repositories are onboarded into the embedding index.
type Indexing struct {
	// OnboardWindow is how many onboarding index jobs the leader keeps
	// queued or running at once; tenants take turns, and the repositories
	// whose pull requests moved last go first.
	OnboardWindow int `yaml:"onboardWindow,omitempty"`
}

// DefaultOnboardWindow applies when the file sets no onboardWindow.
const DefaultOnboardWindow = 4

// Retention controls what is deleted and when. Reviews, findings and usage
// are kept indefinitely; only bulky, reproducible data expires.
type Retention struct {
	// DisabledIndexGrace is how long a disabled repository's vectors are
	// kept before deletion, so re-enabling within the window reuses the
	// index instead of rebuilding it.
	DisabledIndexGrace time.Duration `yaml:"disabledIndexGrace,omitempty"`
	// Transcripts is how long an agentic review's transcript is kept.
	Transcripts time.Duration `yaml:"transcripts,omitempty"`
}

// DefaultDisabledIndexGrace applies when the file sets no retention.
const DefaultDisabledIndexGrace = 30 * 24 * time.Hour

// DefaultTranscripts applies when the file sets no transcript retention.
const DefaultTranscripts = 30 * 24 * time.Hour

// minTranscripts is the shortest transcript retention the file may set.
const minTranscripts = 24 * time.Hour

// TranscriptsOrDefault returns the transcript retention or its default.
func (r Retention) TranscriptsOrDefault() time.Duration {
	if r.Transcripts > 0 {
		return r.Transcripts
	}
	return DefaultTranscripts
}

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

// GitHubApp is a GitHub App credential owned by a connection. The client
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

// Connection is one GitHub App serving the accounts it lists. Its name is
// the hook path, /hooks/{name}, and must be unique across the whole file.
type Connection struct {
	Name  string `yaml:"name"`
	Forge Forge  `yaml:"forge"`
	// Accounts are the users and organizations the connection serves: a
	// webhook for any other account is ignored, and a repository belongs to
	// the connection serving its owner. A public GitHub App installed on
	// several organizations lists each one kritik reviews for; nothing is
	// served that is not listed.
	Accounts []string `yaml:"accounts"`

	App *GitHubApp `yaml:"app,omitempty"`
}

// WebhookSecretValue returns the resolved webhook secret.
func (i Connection) WebhookSecretValue() Secret { return i.App.webhookSecret }

// Repository carries per-repository overrides. Everything a connection
// grants access to is watched whether or not it is listed here.
type Repository struct {
	Name string `yaml:"name"`
	// Connection names the tenant's connection the repository belongs
	// to. It is required only when the owner is the account of more than
	// one connection, so the same "owner/repo" on two forges is two
	// entries.
	Connection string `yaml:"connection,omitempty"`
	Enabled    *bool  `yaml:"enabled,omitempty"`
	Overrides  `yaml:",inline"`
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

// Review is the operator's resolved presentation and strictness for a
// repository. Paths name files in the repository's merge-base tree.
type Review struct {
	Instructions        []string
	RequireSuggestedFix bool
	Templates           ReviewTemplates
	// MinSeverity is the least severe finding posted inline, nit or
	// important; empty posts every one. A blocking finding is always
	// posted, and the summary counts every finding.
	MinSeverity string
	// InlineComments is false to post the summary alone.
	InlineComments bool
	Context        []ContextFile
}

// ContextFile is a repository file that explains the code, named to the
// reviewer with what it is: an agentic review is pointed at it, and a
// single-shot one is given its content. With Paths it applies only when a
// changed path matches one of them.
type ContextFile struct {
	Path        string   `yaml:"path" json:"path"`
	Description string   `yaml:"description" json:"description"`
	Paths       []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// Inline severity floors.
const (
	SeverityNit       = "nit"
	SeverityImportant = "important"
)

// ReviewSpec sets the review block at one scope, field by field: a field
// written here, even empty, replaces the broader scope's.
type ReviewSpec struct {
	Instructions        []string      `yaml:"instructions,omitempty"`
	RequireSuggestedFix *bool         `yaml:"requireSuggestedFix,omitempty"`
	Templates           TemplatesSpec `yaml:"templates,omitempty"`
	MinSeverity         *string       `yaml:"minSeverity,omitempty"`
	InlineComments      *bool         `yaml:"inlineComments,omitempty"`
	Context             []ContextFile `yaml:"context,omitempty"`
}

// TemplatesSpec sets the comment templates at one scope; an empty path
// written here restores the built-in template.
type TemplatesSpec struct {
	Summary *string `yaml:"summary,omitempty"`
	Inline  *string `yaml:"inline,omitempty"`
}

// Referenced lists the repository paths the block names: instructions
// first, then the summary and inline templates and the context files,
// deduplicated.
func (r Review) Referenced() []string {
	paths := append(append([]string(nil), r.Instructions...), r.Templates.Summary, r.Templates.Inline)
	for _, c := range r.Context {
		paths = append(paths, c.Path)
	}
	var out []string
	for _, p := range paths {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Tenant is a forge account and the unit of isolation.
type Tenant struct {
	Slug         string       `yaml:"slug"`
	Runner       *Runner      `yaml:"runner,omitempty"`
	Connections  []Connection `yaml:"connections"`
	Overrides    `yaml:",inline"`
	Limits       LimitsSpec   `yaml:"limits,omitempty"`
	Repositories []Repository `yaml:"repositories,omitempty"`
	// Providers are the tenant's own model providers: its keys, for the
	// models it pays for. A model reference in the tenant names one of them
	// or one of the file's, and a name may not be both.
	Providers map[string]Provider `yaml:"providers,omitempty"`

	origin Origin
}

// Egress is what runner pods may reach through the worker's gateway beyond
// the forges the file itself names, which are always allowed. Hosts are exact, or a suffix with a leading "*."; the gateway
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
	Polling   Polling             `yaml:"polling,omitempty"`
	Indexing  Indexing            `yaml:"indexing,omitempty"`
	Tools     []Tool              `yaml:"tools,omitempty"`
	Retention Retention           `yaml:"retention,omitempty"`
	Egress    Egress              `yaml:"egress,omitempty"`
	Auth      Auth                `yaml:"auth,omitempty"`
	Tenants   []Tenant            `yaml:"tenants"`

	hash string
	// base is the parsed file a merged File was built from, nil for a
	// parsed one; dashboard holds the tenants merged into it.
	base      *File
	dashboard []DashboardTenant
	skipped   []SkippedTenant
}

// Hash is the hex SHA-256 of the file's bytes as parsed, or for a merged
// File, of the parsed file's hash and each dashboard tenant's revision. The
// leader records it in the store after applying the file, and followers
// compare it with their own copy to report drift.
func (f *File) Hash() string { return f.hash }

// Settings are the effective settings for one repository after defaults,
// tenant and repository layers are merged.
type Settings struct {
	Enabled bool
	Models  Models
	Filter  *prfilter.Program
	Forks   bool
	Limits  Limits
	// Ignore is DefaultIgnore plus the repository's own globs. The in-repo
	// file's globs are unioned in by the caller that has the checkout.
	Ignore []string
	// Settle delays a review job for a new head; zero means immediate.
	Settle      time.Duration
	Mode        ReviewMode
	Agent       AgentSettings
	Incremental IncrementalSettings
	Review      Review
	// Allow is the bounds as the narrowest scope writing each one set it;
	// one no scope writes is left unset.
	Allow Allow
}
