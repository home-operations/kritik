// Package configfile is kritik's configuration: the configuration file
// (ADR-0019, ADR-0021) with its KRITIK_* environment overlay, and the
// settings for how kritik runs that come from the environment alone
// (ADR-0021 §2.7). Process configuration (addresses, database, log level)
// is environment variables too and lives in internal/config.
//
// Each layer is applied atomically: the whole document is decoded with
// unknown keys rejected, every secret reference resolved, every filter
// compiled and smoke-tested, and every invariant checked before any of it
// is returned. A bad document is an error and the caller keeps the last
// good state.
package configfile

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

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

// SecretRef names the environment variable a secret value lives in
// (ADR-0022 §2.2). Values are resolved at load and never written back to
// disk or the database.
type SecretRef struct {
	Env string `yaml:"env,omitempty"`
}

// ValueOrRef is a setting given inline, or by a reference to where it
// lives like a secret's, for a value that is not secret but is often kept
// next to one.
type ValueOrRef struct {
	Value string
	Ref   SecretRef
}

// UnmarshalYAML takes a scalar as the value, or a mapping as the reference.
func (v *ValueOrRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&v.Value)
	}
	// Node.Decode drops the strictness Parse asked for, so unknown keys are
	// refused here.
	for i := 0; n.Kind == yaml.MappingNode && i < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Value != "env" {
			return fmt.Errorf("line %d: field %s not found in type configfile.SecretRef", k.Line, k.Value)
		}
	}
	return n.Decode(&v.Ref)
}

// resolve is the value, read from its reference when it has one.
func (v ValueOrRef) resolve(s *secrets) (string, error) {
	if v.Ref.empty() {
		return v.Value, nil
	}
	secret, err := s.read(v.Ref)
	return secret.Value(), err
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

// DefaultRunnerDeadline bounds a runner Job when KRITIK_RUNNER_DEADLINE
// sets none.
const DefaultRunnerDeadline = 15 * time.Minute

// Defaults apply to every repository unless an entry overrides them, and
// Limits to every account unless its entry sets its own.
type Defaults struct {
	Overrides `yaml:",inline"`
	Limits    LimitsSpec `yaml:"limits,omitempty"`
}

// Overrides are the repository settings every admin scope may set: the
// defaults, an account and a repository entry. A field a narrower scope
// writes replaces the broader scope's, even when it is empty or zero; a
// field it leaves out inherits (ADR-0010 §2.4). Ignore globs are unioned
// instead. The review keys are the ones a .kritik.yaml takes too, at the
// same level (ADR-0021 §2.1).
type Overrides struct {
	// Enabled is where a repository starts, on or off, until an admin turns
	// it on or off in the dashboard (ADR-0019 §2.3). A repository entry may
	// not set it.
	Enabled *bool      `yaml:"enabled,omitempty"`
	Models  ModelsSpec `yaml:"models,omitempty"`
	// FilterExpr is CEL over pr; a key whose value is CEL ends in Expr.
	FilterExpr *string  `yaml:"filterExpr,omitempty"`
	Forks      *bool    `yaml:"forks,omitempty"`
	Ignore     []string `yaml:"ignore,omitempty"`
	// Settle delays a review job for a new head, so a burst of pushes
	// collapses onto the last one before anything is spent.
	Settle      *time.Duration `yaml:"settle,omitempty"`
	Mode        ReviewMode     `yaml:"mode,omitempty"`
	Agent       Agent          `yaml:"agent,omitempty"`
	Incremental Incremental    `yaml:"incremental,omitempty"`
	Review      ReviewSpec     `yaml:",inline"`

	filter *prfilter.Program
}

// Tool is a command-line tool a runner pod mounts from an image, read-only,
// for the agent's run tool (ADR-0011). The runner image's own tools (curl,
// fd and rg in the -tools image) need no entry.
type Tool struct {
	// Name identifies the tool; it names the pod volume.
	Name string `json:"name"`
	// Image is the image the tool comes from; pin it by digest.
	Image string `json:"image"`
	// Path is the directory inside the image that holds the binaries; it
	// goes first on the runner's PATH. Default "/". The binaries must be
	// statically linked: the default runner image has no libc.
	Path string `json:"path,omitempty"`
	// Commands are the binaries the tool provides, the names agent.commands
	// allows; default the tool's name.
	Commands []string `json:"commands,omitempty"`
}

// Provides lists the commands the tool puts on the runner's PATH.
func (t Tool) Provides() []string {
	if len(t.Commands) == 0 {
		return []string{t.Name}
	}
	return t.Commands
}

// Embedding is the instance's embedder, which builds the similar-code
// index: a model of one of the instance's openrouter or openai providers,
// on its endpoint and key (ADR-0021 §2.2). There is one per instance
// because the index has one vector dimension (ADR-0014 §2.6). Unset,
// indexing is off and reviews run without vector retrieval.
type Embedding struct {
	// Ref is the model as the configuration names it, "<provider>/<model>".
	Ref ModelRef `yaml:"model"`
	// Dims is the vector dimension, which shapes the index table: a change
	// of it or of the model rebuilds every repository's index.
	Dims int `yaml:"dims"`
	// MaxBatch, MaxBatchChars and MaxItemChars bound one request: inputs,
	// characters, and characters per input, beyond which an input is cut.
	// OpenAI-compatible servers differ widely in what they accept; unset,
	// each is a default conservative enough for the common ones.
	MaxBatch      int `yaml:"maxBatch,omitempty"`
	MaxBatchChars int `yaml:"maxBatchChars,omitempty"`
	MaxItemChars  int `yaml:"maxItemChars,omitempty"`

	// BaseURL, Model and the key are the provider's, resolved at load:
	// its endpoint, the model's id on it, and its key.
	BaseURL string `yaml:"-"`
	Model   string `yaml:"-"`
	apiKey  Secret
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

// minTranscripts is the shortest transcript retention that may be set.
const minTranscripts = 24 * time.Hour

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
// be given inline or by reference.
type GitHubApp struct {
	ClientID      ValueOrRef `yaml:"clientId"`
	PrivateKey    SecretRef  `yaml:"privateKey"`
	WebhookSecret SecretRef  `yaml:"webhookSecret"`

	clientID      string
	privateKey    Secret
	webhookSecret Secret
}

// ClientIDValue returns the client id, inline or resolved.
func (a GitHubApp) ClientIDValue() string { return a.clientID }

// PrivateKeyValue returns the resolved private key PEM.
func (a GitHubApp) PrivateKeyValue() Secret { return a.privateKey }

// Connection is one GitHub App serving the accounts it lists, an entry of
// the configuration's apps (ADR-0021 §2.2). Its name is the hook path,
// /hooks/{name}, and is unique across the configuration.
type Connection struct {
	Name string `yaml:"name"`
	// Forge is always ForgeGitHub; the file does not name it.
	Forge Forge `yaml:"-"`
	// Accounts are the users and organizations the connection serves: a
	// webhook for any other account is ignored, and a repository belongs to
	// the connection serving its owner. A public GitHub App installed on
	// several organizations lists each one kritik reviews for; nothing is
	// served that is not listed.
	Accounts []string `yaml:"accounts"`

	App GitHubApp `yaml:",inline"`
}

// WebhookSecretValue returns the resolved webhook secret.
func (i Connection) WebhookSecretValue() Secret { return i.App.webhookSecret }

// Repository carries per-repository overrides, the configuration's
// owner/name entry for it. Everything a connection grants access to is
// watched whether or not it has one.
type Repository struct {
	// Name is the repository's name under its account, without the owner.
	Name string `yaml:"-"`
	// Overrides keep their yaml names, which the policy table looks them
	// up by, though the entry is decoded as the file's repositories map.
	Overrides `yaml:",inline"`

	// where names the entry in errors.
	where string
}

// RepoTraits is what kritik knows of a repository beyond its name: what
// the forge says of it, that an archived repository is read-only and a
// fork a copy of another one, and whether an admin turned it on or off
// from the dashboard, nil until one does (ADR-0019 §2.3).
type RepoTraits struct {
	Archived, Fork bool
	TurnedOn       *bool
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
	RequireSuggestedFix bool
	Templates           ReviewTemplates
	// InlineComments is false to post the summary alone.
	InlineComments bool
	Context        []ContextFile
	// Rules are the checks the configuration writes, the broadest scope's
	// first (ADR-0018).
	Rules []Rule
	// Feedback is how much a review says (ADR-0021 §2.4): FeedbackDetailed,
	// FeedbackStandard or FeedbackMinimal.
	Feedback string
	// AgentFiles is true to add the repository's AGENTS.md files, or a
	// directory's CLAUDE.md where it has none, to the instructions: the
	// root's and those of the directories a change touches (ADR-0020).
	AgentFiles bool
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

// Feedback levels.
const (
	// FeedbackDetailed reports anything a maintainer could act on, nits,
	// missing tests and questions included, each inline.
	FeedbackDetailed = "detailed"
	// FeedbackStandard is the same review with nits left out of the inline
	// comments; the summary still lists them.
	FeedbackStandard = "standard"
	// FeedbackMinimal reports only bugs, risks and breaking changes.
	FeedbackMinimal = "minimal"
)

// Focused reports whether the reviewer is told to report only what would
// stop the review.
func (r Review) Focused() bool { return r.Feedback == FeedbackMinimal }

// NitsInline reports whether a nit is posted as an inline comment.
func (r Review) NitsInline() bool { return r.Feedback != FeedbackStandard }

// ReviewSpec sets the review keys at one scope, field by field: a field
// written here, even empty, replaces the broader scope's. Rules are the
// exception: they add to the broader scope's, one with an id already
// listed replacing that rule where it stands.
type ReviewSpec struct {
	Feedback            *string       `yaml:"feedback,omitempty"`
	Comments            CommentsSpec  `yaml:"comments,omitempty"`
	RequireSuggestedFix *bool         `yaml:"requireSuggestedFix,omitempty"`
	Rules               []Rule        `yaml:"rules,omitempty"`
	Context             []ContextFile `yaml:"context,omitempty"`
	AgentFiles          *bool         `yaml:"agentFiles,omitempty"`
}

// CommentsSpec sets how a review comments at one scope: whether findings
// go inline, and the repository files that replace the built-in summary
// and inline templates, where an empty path restores the built-in one.
type CommentsSpec struct {
	Inline          *bool   `yaml:"inline,omitempty"`
	SummaryTemplate *string `yaml:"summaryTemplate,omitempty"`
	InlineTemplate  *string `yaml:"inlineTemplate,omitempty"`
}

// Referenced lists the repository paths the block names: the rules'
// files first, then the summary and inline templates and the context
// files, deduplicated.
func (r Review) Referenced() []string {
	paths := make([]string, 0, len(r.Rules)+2+len(r.Context))
	for _, rule := range r.Rules {
		paths = append(paths, rule.File)
	}
	paths = append(paths, r.Templates.Summary, r.Templates.Inline)
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
// (ADR-0014 §2.4). It exists because a connection serves it. Its entry
// under the configuration's accounts holds what is its alone, its limits
// and providers, and its owner/* and owner/name entries under repositories
// its repositories' settings (ADR-0021 §2.2); an account with neither
// inherits the defaults.
type Account struct {
	Forge Forge  `yaml:"-"`
	Name  string `yaml:"-"`
	// Overrides are its owner/* entry's. They and Limits keep their yaml
	// names, which the policy table looks them up by, though the account is
	// gathered from the file's accounts and repositories maps.
	Overrides `yaml:",inline"`
	// Limits and Providers are its accounts entry's: its caps, and its own
	// model providers, its keys for the models it pays for. A model
	// reference for its repositories names one of them or one of the
	// instance's, and a name may not be both.
	Limits    LimitsSpec          `yaml:"limits"`
	Providers map[string]Provider `yaml:"-"`
	// Repositories are its owner/name entries.
	Repositories []Repository `yaml:"-"`

	// entry and pattern name its accounts entry and its owner/* entry in
	// errors, "" for one it does not have.
	entry, pattern string
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

// File is the running configuration, as the configuration file and its
// environment set it (ADR-0019 §2.1).
type File struct {
	Auth        Auth
	Connections []Connection
	Providers   map[string]Provider
	Defaults    Defaults
	Egress      Egress
	// Embedding is the instance's embedder, nil when indexing is off.
	Embedding *Embedding
	// Run is how kritik runs, from the environment (ADR-0021 §2.7).
	Run Run
	// Accounts are every account a running connection serves, in the order
	// the connections list them.
	Accounts []Account

	hash string
	// unserved are the account entries no connection serves.
	unserved []Account
	// envConnection names the connection the environment declared, "" for
	// none.
	envConnection string
	// envProvider names the provider the environment declared, "" for none,
	// and envKeys holds the instance defaults it set, by dotted path.
	envProvider string
	envKeys     map[string]bool
	// secretEnv are the variables f's secrets came from, sorted.
	secretEnv []string
}

// SecretEnv is the environment variables f's secrets came from, sorted,
// which main removes from its environment once f is loaded (ADR-0022
// §2.2).
func (f *File) SecretEnv() []string { return f.secretEnv }

// Hash is the hex SHA-256 of the file's bytes as parsed. The leader records
// it in the store after applying the configuration, and followers compare
// it with their own copy to report drift.
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
	// Providers name the model providers the repository's account may use,
	// the instance's and its own, sorted: the ones a .kritik.yaml may
	// choose a model of (ADR-0021 §2.3).
	Providers []string
}
