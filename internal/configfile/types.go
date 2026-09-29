// Package configfile is kritik's configuration in two layers (ADR-0014
// §2.2): the configuration file, with its KRITIK_AUTH_* and
// KRITIK_CONNECTIONS_* environment overlay, declares sign-in and the
// connections an admin wants fixed at deploy time; the instance spec, which
// the dashboard keeps in Postgres, holds everything else. Process
// configuration (addresses, database, log level) is environment variables
// and lives in internal/config.
//
// Each layer is applied atomically: the whole document is decoded with
// unknown keys rejected, every secret reference resolved, every filter
// compiled and smoke-tested, and every invariant checked before any of it
// is returned. A bad document is an error and the caller keeps the last
// good state.
package configfile

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/home-operations/kritik/internal/agent"
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
// disk or the database. Sealed is ciphertext only the instance spec may
// carry; Env and File would read the server's own environment and
// filesystem, so only the admin's file may use them.
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
// the account's or the instance's providers map and model is whatever the
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
// means the feature is off. The embedding model is not here: it is the
// instance's Embedding, since changing it reindexes every repository.
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

// Limits bound what an account may consume, as resolved. A cap of zero is no
// cap; Concurrency is never zero once resolved.
type Limits struct {
	// Concurrency is the number of advisory-lock slots per account and model:
	// how many model calls may run at once.
	Concurrency int
	// ReviewsPerDay caps review passes per account per calendar day.
	ReviewsPerDay int
	// TokensPerMonth caps input plus output tokens per account per calendar
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

// DefaultConcurrency applies when no level of the configuration sets one.
const DefaultConcurrency = 2

// Runner overrides for the Kubernetes Job an account's index and review pods
// run as. Kept as loose maps for resources because the values are copied
// verbatim into the pod spec and the Kubernetes types are not a dependency
// of this package.
type Runner struct {
	Resources             map[string]any `yaml:"resources,omitempty"`
	ActiveDeadlineSeconds int64          `yaml:"activeDeadlineSeconds,omitempty"`
}

// DefaultRunnerDeadline bounds a runner Job when neither the account nor
// defaults.runner sets activeDeadlineSeconds.
const DefaultRunnerDeadline = 15 * time.Minute

// Defaults apply to every account unless overridden.
type Defaults struct {
	// Runner is every account's runner block unless the account sets its own
	// deadline or resources.
	Runner    *Runner `yaml:"runner,omitempty"`
	Overrides `yaml:",inline"`
	Limits    LimitsSpec `yaml:"limits,omitempty"`
}

// Overrides are the repository settings every admin scope may set: the
// defaults, an account and a repository entry. A field a narrower scope
// writes replaces the broader scope's, even when it is empty or zero; a
// field it leaves out inherits (ADR-0010 §2.4). Ignore globs are unioned
// instead.
type Overrides struct {
	// Enabled turns reviews and indexing on or off. Written at the defaults
	// or an account, it is where each repository without its own entry
	// starts, so an account can take repositories one at a time.
	Enabled *bool      `yaml:"enabled,omitempty"`
	Models  ModelsSpec `yaml:"models,omitempty"`
	Filter  *string    `yaml:"filter,omitempty"`
	Forks   *bool      `yaml:"forks,omitempty"`
	Ignore  []string   `yaml:"ignore,omitempty"`
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
// repository only the admin's own mode, model and commands, and limits
// and a settle time at or below the admin's own.
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

// Poll defaults, when the configuration sets none.
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
	// queued or running at once; accounts take turns, and the repositories
	// whose pull requests moved last go first.
	OnboardWindow int `yaml:"onboardWindow,omitempty"`
}

// DefaultOnboardWindow applies when the configuration sets no onboardWindow.
const DefaultOnboardWindow = 4

// Embedding is the instance's embedder, any OpenAI-compatible embeddings
// endpoint, which builds the similar-code index. There is one per instance
// because the index has one vector dimension (ADR-0014 §2.6). Unset,
// indexing is off and reviews run without vector retrieval.
type Embedding struct {
	// BaseURL is the endpoint, such as https://openrouter.ai/api/v1.
	BaseURL string    `yaml:"baseUrl"`
	APIKey  SecretRef `yaml:"apiKey"`
	Model   string    `yaml:"model"`
	// Dims is the vector dimension, which shapes the index table: a change
	// of it or of Model rebuilds every repository's index.
	Dims int `yaml:"dims"`
	// MaxBatch, MaxBatchChars and MaxItemChars bound one request: inputs,
	// characters, and characters per input, beyond which an input is cut.
	// OpenAI-compatible servers differ widely in what they accept; unset,
	// each is a default conservative enough for the common ones.
	MaxBatch      int `yaml:"maxBatch,omitempty"`
	MaxBatchChars int `yaml:"maxBatchChars,omitempty"`
	MaxItemChars  int `yaml:"maxItemChars,omitempty"`

	apiKey Secret
}

// APIKeyValue returns the resolved API key.
func (e *Embedding) APIKeyValue() Secret { return e.apiKey }

// MaxEmbedDims is the largest dimension the index's halfvec column takes.
const MaxEmbedDims = 4000

// Bounds returns MaxBatch, MaxBatchChars and MaxItemChars, each the
// embedder's default when unset.
func (e *Embedding) Bounds() (batch, batchChars, itemChars int) {
	return cmp.Or(e.MaxBatch, model.DefaultEmbedMaxBatch), cmp.Or(e.MaxBatchChars, model.DefaultEmbedMaxBatchChars),
		cmp.Or(e.MaxItemChars, model.DefaultEmbedMaxItemChars)
}

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

// DefaultTranscripts applies when the configuration sets no transcript
// retention.
const DefaultTranscripts = 30 * 24 * time.Hour

// minTranscripts is the shortest transcript retention the configuration
// may set.
const minTranscripts = 24 * time.Hour

// TranscriptsOrDefault returns the transcript retention or its default.
func (r Retention) TranscriptsOrDefault() time.Duration {
	if r.Transcripts > 0 {
		return r.Transcripts
	}
	return DefaultTranscripts
}

// DefaultIgnore is always skipped by chunking and the caller search, on top
// of whatever the admin's file and the in-repo file add. Vendored and
// generated trees otherwise dominate both.
var DefaultIgnore = []string{
	"vendor/**",
	"node_modules/**",
	"**/*.lock",
	"**/package-lock.json",
	"**/go.sum",
}

// GitHubApp is a GitHub App credential owned by a connection. The client
// id is not secret, but admins often keep it next to the key, so it may
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
// the hook path, /hooks/{name}, and is unique across the configuration.
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

	origin Origin
}

// WebhookSecretValue returns the resolved webhook secret.
func (i Connection) WebhookSecretValue() Secret { return i.App.webhookSecret }

// Repository carries per-repository overrides. Everything a connection
// grants access to is watched whether or not it is listed here.
type Repository struct {
	// Name is the repository's name under its account, without the owner.
	Name      string `yaml:"name"`
	Overrides `yaml:",inline"`
}

// RepoTraits is what the forge says of a repository beyond its name: an
// archived repository is read-only, and a fork is a copy of another one.
type RepoTraits struct {
	Archived, Fork bool
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
// steps, tool output and tokens are the agent loop's own defaults. No
// command is allowed by default: a repository is opted into the run tool.
var DefaultAgent = AgentSettings{
	MaxSteps:           agent.DefaultLimits.MaxSteps,
	MaxToolOutputBytes: agent.DefaultLimits.MaxToolOutputBytes,
	MaxTokens:          agent.DefaultLimits.MaxTokens,
	Timeout:            20 * time.Minute,
	CommandTimeout:     30 * time.Second,
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

// Review is the admin's resolved presentation and strictness for a
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
	// Thoroughness is what the reviewer reports: ThoroughnessThorough,
	// anything a maintainer could act on, or ThoroughnessFocused, only what
	// would stop the review.
	Thoroughness string
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

// Review thoroughnesses.
const (
	ThoroughnessThorough = "thorough"
	ThoroughnessFocused  = "focused"
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
	Thoroughness        *string       `yaml:"thoroughness,omitempty"`
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

// Account is a forge account, github/<name>, and the unit of isolation
// (ADR-0014 §2.4). It exists because a connection serves it; its entry in
// the spec, if any, holds its settings, and one the spec does not list
// inherits the defaults.
type Account struct {
	Forge        Forge   `yaml:"forge"`
	Name         string  `yaml:"name"`
	Runner       *Runner `yaml:"runner,omitempty"`
	Overrides    `yaml:",inline"`
	Limits       LimitsSpec   `yaml:"limits,omitempty"`
	Repositories []Repository `yaml:"repositories,omitempty"`
	// Providers are the account's own model providers: its keys, for the
	// models it pays for. A model reference in the account names one of them
	// or one of the instance's, and a name may not be both.
	Providers map[string]Provider `yaml:"providers,omitempty"`
}

// Egress is what runner pods may reach through the worker's gateway beyond
// the forges the connections talk to, which are always allowed. Hosts are
// exact, or a suffix with a leading "*."; the gateway
// tunnels TLS to port 443 only. A credential is the token the gateway adds,
// as a bearer, to a plain http:// request a runner makes to that host, so
// the runner can use an API at a token's rate limit without holding it.
type Egress struct {
	AllowHosts  []string             `yaml:"allowHosts,omitempty"`
	Credentials map[string]SecretRef `yaml:"credentials,omitempty"`

	credentials map[string]Secret
}

// Spec is the instance configuration the dashboard keeps in Postgres: every
// setting but sign-in and the connections the file declares. Its secrets
// are sealed.
type Spec struct {
	Providers   map[string]Provider `yaml:"providers,omitempty"`
	Defaults    Defaults            `yaml:"defaults,omitempty"`
	Polling     Polling             `yaml:"polling,omitempty"`
	Indexing    Indexing            `yaml:"indexing,omitempty"`
	Tools       []Tool              `yaml:"tools,omitempty"`
	Retention   Retention           `yaml:"retention,omitempty"`
	Egress      Egress              `yaml:"egress,omitempty"`
	Embedding   *Embedding          `yaml:"embedding,omitempty"`
	Connections []Connection        `yaml:"connections,omitempty"`
	Accounts    []Account           `yaml:"accounts,omitempty"`
}

// File is the running configuration: the configuration file's sign-in and
// connections, and the instance spec merged over them. A File returned by
// Parse carries only the file's layer; Merge adds the spec.
type File struct {
	Auth Auth
	// Connections are the file's and the spec's that run.
	Connections []Connection
	Providers   map[string]Provider
	Defaults    Defaults
	Polling     Polling
	Indexing    Indexing
	Tools       []Tool
	Retention   Retention
	Egress      Egress
	// Embedding is the instance's embedder, nil when indexing is off.
	Embedding *Embedding
	// Accounts are every account a running connection serves, in the order
	// the connections list them.
	Accounts []Account

	hash string
	// base is the parsed file a merged File was built from, nil for a
	// parsed one.
	base *File
	spec InstanceSpec
	// skipped are the file's connections the spec's crowd out, and
	// unserved the spec's accounts no running connection serves.
	skipped  []SkippedConnection
	unserved []Account
	// envConnection names the connection the environment declared, "" for
	// none.
	envConnection string
}

// Hash is the hex SHA-256 of the file's bytes as parsed, or for a merged
// File, of the parsed file's hash and the spec's revision and content. The
// leader records it in the store after applying the configuration, and
// followers compare it with their own copy to report drift.
func (f *File) Hash() string { return f.hash }

// Settings are the effective settings for one repository after defaults,
// account and repository layers are merged.
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
