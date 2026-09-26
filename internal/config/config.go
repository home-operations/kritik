// Package config loads kritik's process configuration from environment
// variables. Everything the service manages (tenants, installations,
// repositories, models) lives in the declarative configuration file, not
// here; this package covers only what the process itself needs to start.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/home-operations/kritik/internal/egress"
	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/sealbox"
)

// Role selects which part of kritik a process runs. One image serves every
// role; a homelab runs "all" in one pod while a larger deployment runs
// "ingest" and "worker" as separate Deployments and lets the worker spawn
// "runner" Jobs.
type Role string

// Roles a kritik process can run as.
const (
	RoleAll    Role = "all"
	RoleIngest Role = "ingest"
	RoleWorker Role = "worker"
	RoleRunner Role = "runner"
	// RoleWeb serves the operator dashboard (ADR-0009): sign-in, sessions
	// and the tenant/config surfaces a dashboard-managed installation uses.
	// "all" also serves it once WebURL is configured; see [Config.WebEnabled].
	RoleWeb Role = "web"
)

// ParseRole validates a role name from the command line.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case RoleAll, RoleIngest, RoleWorker, RoleRunner, RoleWeb:
		return r, nil
	default:
		return "", fmt.Errorf("config: unknown role %q (want all, ingest, worker, runner or web)", s)
	}
}

// Config holds the process configuration for kritik. All fields are populated
// from environment variables via caarlos0/env. Call [Load] to parse and
// validate; do not construct directly.
type Config struct {
	// Addr is the listen address for the HTTP surface the ingest role serves:
	// /hooks/{installation} and nothing else. Port 8080 matches the container
	// image's EXPOSE and the other services in the fleet.
	Addr string `env:"KRITIK_ADDR" envDefault:":8080"`

	// MetricsAddr is the listen address for /healthz, /readyz and /metrics.
	// Kept on a separate port from the hook surface, as konflate does, so the
	// management endpoints are never reachable through the ingress that fronts
	// the webhooks.
	MetricsAddr string `env:"KRITIK_METRICS_ADDR" envDefault:":8081"`

	// GatewayAddr is the listen address of the gateway the worker and all
	// roles serve: the forward proxy runner pods reach the outside through
	// (ADR-0008), and the model endpoint an agentic runner calls with its
	// run token (ADR-0004). Its own port, so the runner network policy can
	// name it without opening the hook or management surfaces.
	GatewayAddr string `env:"KRITIK_GATEWAY_ADDR" envDefault:":8082"`

	// GatewayURL is the gateway's in-cluster address, http://host:port:
	// runner Jobs are handed it as HTTPS_PROXY and HTTP_PROXY, and an
	// agentic runner's job document names it as its model endpoint. Empty
	// hands runners no proxy, which leaves them the direct egress an older
	// network policy allowed, and refuses agentic reviews.
	GatewayURL string `env:"KRITIK_GATEWAY_URL"`

	// GatewayTokenTTL is how long a run token outlives its runner Job's
	// deadline before it expires on its own, in case the worker that
	// minted it dies before revoking it.
	GatewayTokenTTL time.Duration `env:"KRITIK_GATEWAY_TOKEN_TTL" envDefault:"1h"`

	// WebAddr is the listen address for the operator dashboard the web role
	// serves. Its own port, matching the pattern of Addr/MetricsAddr/
	// GatewayAddr, so the dashboard can be exposed without opening the
	// other surfaces.
	WebAddr string `env:"KRITIK_WEB_ADDR" envDefault:":8083"`

	// WebURL is the dashboard's externally reachable origin: an absolute
	// http(s) URL with a host and no query or fragment. It is how the
	// dashboard builds absolute links (OIDC redirect URIs, session cookie
	// scope) back to itself, so it must be required for the web role and
	// must match how the ingress/HTTPRoute actually exposes it. A trailing
	// slash is trimmed. Empty means no role serves the dashboard; see
	// [Config.WebEnabled]. Parsed once into an unexported *url.URL, read
	// back with [Config.WebURLParsed].
	WebURL string `env:"KRITIK_WEB_URL"`

	// ConfigFile is the path of the declarative configuration file (tenants,
	// installations, repositories, models). Every role except runner loads it
	// at startup and watches it for changes. The default is where the Helm
	// chart mounts it.
	ConfigFile string `env:"KRITIK_CONFIG_FILE" envDefault:"/etc/kritik/config.yaml"`

	// ConfigReloadInterval is how often the configuration file is re-read
	// for changes. Polling, because a ConfigMap mount updates by swapping a
	// symlink that inotify on the file misses; ten seconds keeps a Flux
	// reconcile visible without hammering the disk.
	ConfigReloadInterval time.Duration `env:"KRITIK_CONFIG_RELOAD_INTERVAL" envDefault:"10s"`

	// DatabaseURL is the DSN every role connects with for request and job
	// work. It must be the application role: not a superuser, no BYPASSRLS,
	// owning nothing, so row-level security applies to it. Runner pods get
	// the runner role's DSN under the same variable name. Checked at
	// startup; a DSN that bypasses row-level security refuses to start.
	DatabaseURL string `env:"KRITIK_DATABASE_URL,required,notEmpty,unset"`

	// DatabaseOwnerURL is the DSN of the role that owns the schema. It runs
	// migrations, the configuration loader and the chunks DDL, all of which
	// write across tenants. Only the roles that can become leader (all and
	// worker) need it; ingest and runner must not have it.
	DatabaseOwnerURL string `env:"KRITIK_DATABASE_OWNER_URL,unset"`

	// DatabaseAppRole and DatabaseRunnerRole are the Postgres role names the
	// owner grants privileges to during migrations. They default to what
	// deploy/dev and the chart's CloudNativePG example declare.
	DatabaseAppRole    string `env:"KRITIK_DATABASE_APP_ROLE" envDefault:"kritik_app"`
	DatabaseRunnerRole string `env:"KRITIK_DATABASE_RUNNER_ROLE" envDefault:"kritik_runner"`

	// LeaderRetryInterval is how often a leader-eligible replica retries the
	// leader lock and how often the holder verifies it still has it.
	LeaderRetryInterval time.Duration `env:"KRITIK_LEADER_RETRY_INTERVAL" envDefault:"15s"`

	// EmbedBaseURL, EmbedAPIKey, EmbedModel and EmbedDims configure the
	// deployment-wide embedder, always OpenAI-compatible. They live here
	// rather than in the configuration file because changing the model
	// reindexes every repository, so it should take a deploy, not a config
	// reload. All four are set together or not at all; unset means indexing
	// is off and reviews run without vector retrieval.
	EmbedBaseURL string `env:"KRITIK_EMBED_BASE_URL"`
	EmbedAPIKey  string `env:"KRITIK_EMBED_API_KEY,unset"`
	EmbedModel   string `env:"KRITIK_EMBED_MODEL"`
	EmbedDims    int    `env:"KRITIK_EMBED_DIMS"`

	// EmbedMaxBatch, EmbedMaxBatchChars and EmbedMaxItemChars bound one
	// embedding request. OpenAI-compatible servers differ widely in what
	// they accept; these defaults are conservative enough for the common
	// ones and can be raised per deployment.
	EmbedMaxBatch      int `env:"KRITIK_EMBED_MAX_BATCH" envDefault:"64"`
	EmbedMaxBatchChars int `env:"KRITIK_EMBED_MAX_BATCH_CHARS" envDefault:"200000"`
	EmbedMaxItemChars  int `env:"KRITIK_EMBED_MAX_ITEM_CHARS" envDefault:"16000"`

	// ReindexOnModelChange lets a worker start when EmbedModel differs from
	// the model recorded in the store at the same dimension, and enqueues a
	// reindex of every repository. Off by default so a mistyped model name
	// cannot trigger a fleet-wide re-embed.
	ReindexOnModelChange bool `env:"KRITIK_REINDEX_ON_MODEL_CHANGE" envDefault:"false"`

	// PollInterval is how often the leader lists each installation's open
	// pull requests to catch missed webhooks; 0 disables the poll.
	// PollLookback bounds how far back a first or long-idle poll looks.
	PollInterval time.Duration `env:"KRITIK_POLL_INTERVAL" envDefault:"10m"`
	PollLookback time.Duration `env:"KRITIK_POLL_LOOKBACK" envDefault:"24h"`

	// ReviewWorkers is how many review jobs one worker replica runs at once.
	// Each one holds a runner pod open for the length of a fetch and diff,
	// so this bounds pods per replica, not model calls.
	ReviewWorkers int `env:"KRITIK_REVIEW_WORKERS" envDefault:"2"`
	// IndexWorkers is how many index jobs one worker replica runs at once;
	// indexing is rate-limited apart from reviews so onboarding a large
	// account cannot starve them.
	IndexWorkers int `env:"KRITIK_INDEX_WORKERS" envDefault:"1"`

	// Executor selects how runners run: "kubernetes" creates a Job per run
	// in the pod's own namespace; "local" runs the runner in-process and is
	// for development and tests only, because it gives the checkout the
	// worker's credentials.
	Executor string `env:"KRITIK_EXECUTOR" envDefault:"kubernetes"`

	// RunnerImage is the image runner Jobs use, normally the worker's own.
	// Required for the worker and all roles with the kubernetes executor.
	RunnerImage string `env:"KRITIK_RUNNER_IMAGE"`

	// RunnerServiceAccount is the permissionless service account runner pods
	// run as. RunnerDatabaseSecret and RunnerDatabaseSecretKey locate the
	// runner role's DSN, which the Job injects as KRITIK_DATABASE_URL.
	RunnerServiceAccount    string `env:"KRITIK_RUNNER_SERVICE_ACCOUNT" envDefault:"kritik-runner"`
	RunnerDatabaseSecret    string `env:"KRITIK_RUNNER_DATABASE_SECRET" envDefault:"kritik-postgres-runner"`
	RunnerDatabaseSecretKey string `env:"KRITIK_RUNNER_DATABASE_SECRET_KEY" envDefault:"uri"`

	// RunnerDeadline bounds a runner when the tenant sets none, and like a
	// tenant's runner.activeDeadlineSeconds must not exceed
	// jobtimeout.MaxRunnerDeadline, past which River would cut the job off
	// before the deadline does; RunnerTTL is how long a finished Job stays
	// for kubectl before Kubernetes removes it. The run row keeps everything
	// the Job knew.
	RunnerDeadline time.Duration `env:"KRITIK_RUNNER_DEADLINE" envDefault:"15m"`
	RunnerTTL      time.Duration `env:"KRITIK_RUNNER_TTL" envDefault:"10m"`

	// RunnerRuntimeClass is the RuntimeClass runner pods run under, such as
	// a gVisor or Kata class, so a pod that parses untrusted repository
	// content is kept from the node's kernel. Empty uses the cluster's
	// default runtime. Advised, not required (ADR-0008 §2.4).
	RunnerRuntimeClass string `env:"KRITIK_RUNNER_RUNTIME_CLASS"`

	// RunnerDatabaseURL is the runner role's DSN, needed only by the local
	// executor, which runs the runner inside the worker process.
	RunnerDatabaseURL string `env:"KRITIK_RUNNER_DATABASE_URL,unset"`

	// RunSpecFile is the path of the runner role's job document, a
	// versioned JSON runner spec the worker mounts into the Job from the
	// run's Secret. A file rather than a variable: a spec can outgrow the
	// kernel's 128 KiB limit on one environment string.
	RunSpecFile string `env:"KRITIK_RUN_SPEC_FILE"`
	// GitToken and GatewayToken are the runner's credentials, read from
	// the run's own Secret. The gateway token is set only for an agentic
	// review.
	GitToken     string `env:"KRITIK_GIT_TOKEN,unset"`
	GatewayToken string `env:"KRITIK_GATEWAY_TOKEN,unset"`

	// DashboardKey is the base64 32-byte key that seals and opens the
	// credentials a dashboard-managed tenant stores (ADR-0009 §2.5). Empty
	// is valid while no dashboard tenant exists; startup fails once one
	// does. Passed like every other secret here, from the environment and
	// unset once read.
	DashboardKey string `env:"KRITIK_DASHBOARD_KEY,unset"`
	// DashboardOldKeys are earlier DashboardKey values, comma-separated,
	// still able to open what they sealed so a key can be rotated without
	// resealing every tenant first. Empty by default: there is nothing to
	// rotate from until a key has been replaced.
	DashboardOldKeys []string `env:"KRITIK_DASHBOARD_OLD_KEYS,unset" envSeparator:","`

	// LogLevel is the minimum slog level emitted: debug, info, warn or error.
	LogLevel string `env:"KRITIK_LOG_LEVEL" envDefault:"info"`

	// LogFormat selects the slog handler: "json" (the default, for containers)
	// or "text" for local runs.
	LogFormat string `env:"KRITIK_LOG_FORMAT" envDefault:"json"`

	keyring *sealbox.Keyring
	webURL  *url.URL
}

// ValidateWorker checks what the worker role needs beyond the common set.
func (c *Config) ValidateWorker() error {
	if c.Executor == "kubernetes" && c.RunnerImage == "" {
		return fmt.Errorf("config: KRITIK_RUNNER_IMAGE is required with the kubernetes executor")
	}
	if c.Executor == "local" && c.RunnerDatabaseURL == "" {
		return fmt.Errorf("config: KRITIK_RUNNER_DATABASE_URL is required with the local executor")
	}
	return nil
}

// ValidateRunner checks what a runner pod needs. The job document itself is
// decoded and validated by the runner package.
func (c *Config) ValidateRunner() error {
	if c.RunSpecFile == "" {
		return fmt.Errorf("config: KRITIK_RUN_SPEC_FILE is required for the runner role")
	}
	return nil
}

// ValidateWeb checks what the web role needs beyond the common set.
func (c *Config) ValidateWeb() error {
	if c.WebURL == "" {
		return fmt.Errorf("config: KRITIK_WEB_URL is required for the web role")
	}
	return nil
}

// WebEnabled reports whether role serves the operator dashboard: the web
// role always does, and all does once WebURL is configured.
func (c *Config) WebEnabled(role Role) bool {
	return role == RoleWeb || (role == RoleAll && c.WebURL != "")
}

// WebURLParsed returns WebURL parsed into a *url.URL, or nil when WebURL is
// unset.
func (c *Config) WebURLParsed() *url.URL { return c.webURL }

// parseWebURL trims a trailing slash from WebURL, rejects anything that
// isn't an absolute http(s) URL with a host and no query or fragment, and
// caches the result for WebURLParsed. A no-op when WebURL is unset.
func (c *Config) parseWebURL() error {
	if c.WebURL == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(c.WebURL, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("config: KRITIK_WEB_URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: KRITIK_WEB_URL must be an absolute http(s) URL, got %q", c.WebURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config: KRITIK_WEB_URL must not have a query or fragment, got %q", c.WebURL)
	}
	c.WebURL = trimmed
	c.webURL = u
	return nil
}

// EmbeddingEnabled reports whether a deployment-wide embedder is configured.
func (c *Config) EmbeddingEnabled() bool { return c.EmbedModel != "" }

// DashboardKeyring returns the keyring built from DashboardKey and
// DashboardOldKeys, nil when no key is configured.
func (c *Config) DashboardKeyring() *sealbox.Keyring { return c.keyring }

// Load parses the environment into a Config and validates it. It fails fast
// on an invalid value so a misconfigured process never starts serving.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if _, err := c.Level(); err != nil {
		return err
	}
	switch strings.ToLower(c.LogFormat) {
	case "json", "text":
	default:
		return fmt.Errorf("config: KRITIK_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	if c.ConfigReloadInterval <= 0 {
		return fmt.Errorf("config: KRITIK_CONFIG_RELOAD_INTERVAL must be positive, got %s", c.ConfigReloadInterval)
	}
	if c.GatewayURL != "" {
		if _, err := egress.ProxyURL(c.GatewayURL); err != nil {
			return fmt.Errorf("config: KRITIK_GATEWAY_URL: %w", err)
		}
	}
	if c.GatewayTokenTTL <= 0 {
		return fmt.Errorf("config: KRITIK_GATEWAY_TOKEN_TTL must be positive, got %s", c.GatewayTokenTTL)
	}
	if err := c.parseWebURL(); err != nil {
		return err
	}
	set := 0
	for _, v := range []bool{c.EmbedBaseURL != "", c.EmbedAPIKey != "", c.EmbedModel != "", c.EmbedDims != 0} {
		if v {
			set++
		}
	}
	if set != 0 && set != 4 {
		return fmt.Errorf("config: KRITIK_EMBED_BASE_URL, KRITIK_EMBED_API_KEY, KRITIK_EMBED_MODEL and KRITIK_EMBED_DIMS must be set together")
	}
	if c.EmbedDims < 0 || c.EmbedDims > 4000 {
		return fmt.Errorf("config: KRITIK_EMBED_DIMS must be between 1 and 4000 (the halfvec index limit), got %d", c.EmbedDims)
	}
	if c.EmbedMaxBatch <= 0 || c.EmbedMaxBatchChars <= 0 || c.EmbedMaxItemChars <= 0 {
		return fmt.Errorf("config: KRITIK_EMBED_MAX_* must be positive")
	}
	if c.DatabaseAppRole == "" || c.DatabaseRunnerRole == "" || c.DatabaseAppRole == c.DatabaseRunnerRole {
		return fmt.Errorf("config: KRITIK_DATABASE_APP_ROLE and KRITIK_DATABASE_RUNNER_ROLE must be set and distinct")
	}
	if c.LeaderRetryInterval <= 0 {
		return fmt.Errorf("config: KRITIK_LEADER_RETRY_INTERVAL must be positive, got %s", c.LeaderRetryInterval)
	}
	switch c.Executor {
	case "kubernetes", "local":
	default:
		return fmt.Errorf("config: KRITIK_EXECUTOR must be kubernetes or local, got %q", c.Executor)
	}
	if c.PollInterval < 0 || c.PollLookback <= 0 {
		return fmt.Errorf("config: KRITIK_POLL_INTERVAL must not be negative and KRITIK_POLL_LOOKBACK must be positive")
	}
	if c.ReviewWorkers <= 0 || c.IndexWorkers <= 0 || c.RunnerDeadline <= 0 || c.RunnerTTL <= 0 {
		return fmt.Errorf("config: KRITIK_REVIEW_WORKERS, KRITIK_INDEX_WORKERS, KRITIK_RUNNER_DEADLINE and KRITIK_RUNNER_TTL must be positive")
	}
	if err := c.buildKeyring(); err != nil {
		return err
	}
	if c.RunnerDeadline > jobtimeout.MaxRunnerDeadline {
		return fmt.Errorf("config: KRITIK_RUNNER_DEADLINE must not exceed %s (the %s job cap less the review and index headroom), got %s",
			jobtimeout.MaxRunnerDeadline, jobtimeout.MaxJobTimeout, c.RunnerDeadline)
	}
	return nil
}

func (c *Config) buildKeyring() error {
	if c.DashboardKey == "" {
		if len(c.DashboardOldKeys) > 0 {
			return fmt.Errorf("config: KRITIK_DASHBOARD_OLD_KEYS needs KRITIK_DASHBOARD_KEY")
		}
		return nil
	}
	current, err := sealbox.ParseKey(c.DashboardKey)
	if err != nil {
		return fmt.Errorf("config: KRITIK_DASHBOARD_KEY: %w", err)
	}
	old := make([][]byte, 0, len(c.DashboardOldKeys))
	for i, s := range c.DashboardOldKeys {
		k, err := sealbox.ParseKey(s)
		if err != nil {
			return fmt.Errorf("config: KRITIK_DASHBOARD_OLD_KEYS[%d]: %w", i, err)
		}
		old = append(old, k)
	}
	if c.keyring, err = sealbox.NewKeyring(current, old...); err != nil {
		return fmt.Errorf("config: KRITIK_DASHBOARD_KEY: %w", err)
	}
	return nil
}

// Level returns the slog level named by LogLevel.
func (c *Config) Level() (slog.Level, error) {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: KRITIK_LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
	}
}
