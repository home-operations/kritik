// Package config loads kritik's process configuration from environment
// variables: what the process itself needs to start. What kritik reviews
// and how lives in the configuration file and its KRITIK_* overlay
// (internal/configfile).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/home-operations/kritik/internal/egress"
)

// Command selects what a kritik process runs (ADR-0024): the service, or
// one review or index run in a runner Job.
type Command string

// Commands a kritik process can run.
const (
	// CommandServe is the service: webhooks, the dashboard, the job
	// queues, runner Jobs, the gateway and, on the leader, the leader
	// duties.
	CommandServe Command = "serve"
	// CommandRun is one run in a runner Job, which the service creates.
	CommandRun Command = "run"
)

// ParseCommand reads the command from a process's arguments, without the
// program name: serve when there are none.
func ParseCommand(args []string) (Command, error) {
	if len(args) == 0 {
		return CommandServe, nil
	}
	if len(args) == 1 {
		switch c := Command(args[0]); c {
		case CommandServe, CommandRun:
			return c, nil
		}
	}
	return "", fmt.Errorf("config: unknown arguments %q: usage: kritik [serve | run]", args)
}

// Executor selects how runners run.
type Executor string

// Executors a kritik process can run runners with.
const (
	// ExecutorKubernetes creates a Job per run in the pod's own namespace.
	ExecutorKubernetes Executor = "kubernetes"
	// ExecutorLocal runs the runner in-process and is for development and
	// tests only, because it gives the checkout the worker's credentials.
	ExecutorLocal Executor = "local"
)

// Config holds the process configuration for kritik. All fields are populated
// from environment variables via caarlos0/env. Call [Load] to parse and
// validate; do not construct directly.
type Config struct {
	// Addr is the listen address of serve's one public listener (ADR-0024
	// §2.2): the webhooks, /hooks/{connection}, and the dashboard. Port 8080
	// matches the container image's EXPOSE and the other services in the
	// fleet.
	Addr string `env:"KRITIK_ADDR" envDefault:":8080"`

	// MetricsAddr is the listen address for /healthz, /readyz and /metrics.
	// Kept on a separate port from the public listener, as konflate does, so
	// the management endpoints are never reachable through the ingress that
	// fronts it.
	MetricsAddr string `env:"KRITIK_METRICS_ADDR" envDefault:":8081"`

	// GatewayAddr is the listen address of the gateway serve runs: the
	// forward proxy runner pods reach the outside through (ADR-0008), and
	// the model and similar-code endpoints a runner calls with its run
	// token (ADR-0004, ADR-0026). Its own port, so the runner network policy can
	// name it without opening the public or management listeners.
	GatewayAddr string `env:"KRITIK_GATEWAY_ADDR" envDefault:":8082"`

	// GatewayURL is the gateway's in-cluster address, http://host:port:
	// runner Jobs are handed it as HTTPS_PROXY and HTTP_PROXY, and a
	// review's job document names it as its model endpoint. serve needs
	// it, since every review reaches its model through it (ADR-0026).
	GatewayURL string `env:"KRITIK_GATEWAY_URL"`

	// GatewayTokenTTL is how long a run token outlives its runner Job's
	// deadline before it expires on its own, in case the worker that
	// minted it dies before revoking it.
	GatewayTokenTTL time.Duration `env:"KRITIK_GATEWAY_TOKEN_TTL" envDefault:"1h"`

	// WebURL is the dashboard's externally reachable origin: an absolute
	// http(s) URL with a host and no query or fragment. It is how the
	// dashboard builds absolute links (OIDC redirect URIs, session cookie
	// scope) back to itself, so it must match how the ingress/HTTPRoute
	// actually exposes it. A trailing slash is trimmed. Required to serve.
	// Parsed once into an unexported *url.URL, read back with
	// [Config.WebURLParsed].
	WebURL string `env:"KRITIK_WEB_URL"`

	// ConfigFile is the path of the optional configuration file: sign-in
	// and the connections an admin keeps in git. serve loads it at
	// startup, and a change takes a restart (ADR-0022 §2.1).
	// Empty means no file, and the environment alone declares them.
	ConfigFile string `env:"KRITIK_CONFIG_FILE"`

	// DatabaseURL is the DSN every process connects with for request and
	// job work. It must be the application role: not a superuser, no BYPASSRLS,
	// owning nothing, so row-level security applies to it. Runner pods get
	// the runner role's DSN under the same variable name. Checked at
	// startup; a DSN that bypasses row-level security refuses to start.
	DatabaseURL string `env:"KRITIK_DATABASE_URL,required,notEmpty,unset"`

	// DatabaseOwnerURL is the DSN of the role that owns the schema. It runs
	// migrations, the configuration loader and the chunks DDL, all of which
	// write across accounts. serve needs it to become leader; a runner
	// must not have it.
	DatabaseOwnerURL string `env:"KRITIK_DATABASE_OWNER_URL,unset"`

	// DatabaseAppRole and DatabaseRunnerRole are the Postgres role names the
	// owner grants privileges to during migrations. They default to what
	// deploy/dev and the chart's CloudNativePG example declare.
	DatabaseAppRole    string `env:"KRITIK_DATABASE_APP_ROLE" envDefault:"kritik_app"`
	DatabaseRunnerRole string `env:"KRITIK_DATABASE_RUNNER_ROLE" envDefault:"kritik_runner"`

	// LeaderRetryInterval is how often a leader-eligible replica retries the
	// leader lock and how often the holder verifies it still has it.
	LeaderRetryInterval time.Duration `env:"KRITIK_LEADER_RETRY_INTERVAL" envDefault:"15s"`

	// ReviewWorkers is how many review jobs one worker replica runs at once.
	// Each one holds a runner pod open for the length of a fetch and diff,
	// so this bounds pods per replica, not model calls.
	ReviewWorkers int `env:"KRITIK_REVIEW_WORKERS" envDefault:"2"`
	// IndexWorkers is how many index jobs one worker replica runs at once;
	// indexing is rate-limited apart from reviews so onboarding a large
	// account cannot starve them. Each holds a runner pod too, so a replica
	// runs at most ReviewWorkers + IndexWorkers runner pods.
	IndexWorkers int `env:"KRITIK_INDEX_WORKERS" envDefault:"1"`

	// Executor selects how runners run; see the Executor type.
	Executor Executor `env:"KRITIK_EXECUTOR" envDefault:"kubernetes"`

	// RunnerImage is the image runner Jobs use, normally serve's own.
	// Required to serve with the kubernetes executor.
	RunnerImage string `env:"KRITIK_RUNNER_IMAGE"`

	// RunnerServiceAccount is the permissionless service account runner pods
	// run as. RunnerDatabaseSecret and RunnerDatabaseSecretKey locate the
	// runner role's DSN, which the Job injects as KRITIK_DATABASE_URL.
	RunnerServiceAccount    string `env:"KRITIK_RUNNER_SERVICE_ACCOUNT" envDefault:"kritik-runner"`
	RunnerDatabaseSecret    string `env:"KRITIK_RUNNER_DATABASE_SECRET" envDefault:"kritik-postgres-runner"`
	RunnerDatabaseSecretKey string `env:"KRITIK_RUNNER_DATABASE_SECRET_KEY" envDefault:"uri"`

	// RunnerTTL is how long a finished Job stays for kubectl before
	// Kubernetes removes it. The run row keeps everything the Job knew.
	RunnerTTL time.Duration `env:"KRITIK_RUNNER_TTL" envDefault:"10m"`

	// RunnerRuntimeClass is the RuntimeClass runner pods run under, such as
	// a gVisor or Kata class, so a pod that parses untrusted repository
	// content is kept from the node's kernel. Empty uses the cluster's
	// default runtime. Advised, not required (ADR-0008 §2.4).
	RunnerRuntimeClass string `env:"KRITIK_RUNNER_RUNTIME_CLASS"`

	// RunnerDatabaseURL is the runner database role's DSN, needed only by
	// the local executor, which runs the runner inside the serve process.
	RunnerDatabaseURL string `env:"KRITIK_RUNNER_DATABASE_URL,unset"`

	// RunSpecFile is the path of a runner's job document, a
	// versioned JSON runner spec the worker mounts into the Job from the
	// run's Secret. A file rather than a variable: a spec can outgrow the
	// kernel's 128 KiB limit on one environment string.
	RunSpecFile string `env:"KRITIK_RUN_SPEC_FILE"`
	// GitToken and GatewayToken are the runner's credentials, read from
	// the run's own Secret. The gateway token is set only for an agentic
	// review.
	GitToken     string `env:"KRITIK_GIT_TOKEN,unset"`
	GatewayToken string `env:"KRITIK_GATEWAY_TOKEN,unset"`

	// LogLevel is the minimum slog level emitted: debug, info, warn or error.
	LogLevel string `env:"KRITIK_LOG_LEVEL" envDefault:"info"`

	// LogFormat selects the slog handler: "json" (the default, for containers)
	// or "text" for local runs.
	LogFormat string `env:"KRITIK_LOG_FORMAT" envDefault:"json"`

	webURL *url.URL
}

// ValidateServe checks what serve needs beyond the common set.
func (c *Config) ValidateServe() error {
	var errs []error
	if c.Executor == ExecutorKubernetes && c.RunnerImage == "" {
		errs = append(errs, errors.New("config: KRITIK_RUNNER_IMAGE is required with the kubernetes executor"))
	}
	if c.Executor == ExecutorLocal && c.RunnerDatabaseURL == "" {
		errs = append(errs, errors.New("config: KRITIK_RUNNER_DATABASE_URL is required with the local executor"))
	}
	if c.WebURL == "" {
		errs = append(errs, errors.New("config: KRITIK_WEB_URL is required to serve"))
	}
	if c.GatewayURL == "" {
		errs = append(errs, errors.New("config: KRITIK_GATEWAY_URL is required to serve: every review reaches its model through the gateway"))
	}
	return errors.Join(errs...)
}

// ValidateRunner checks what a runner pod needs. The job document itself is
// decoded and validated by the runner package.
func (c *Config) ValidateRunner() error {
	if c.RunSpecFile == "" {
		return errors.New("config: KRITIK_RUN_SPEC_FILE is required to run")
	}
	return nil
}

// WebURLParsed returns WebURL parsed into a *url.URL, or nil when WebURL is
// unset.
func (c *Config) WebURLParsed() *url.URL { return c.webURL }

// WebBasePath is WebURL's path, "" when it has none or is unset: the
// dashboard is served under it, and the webhook listener shares it.
func (c *Config) WebBasePath() string {
	if c.webURL == nil {
		return ""
	}
	return c.webURL.Path
}

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
	if c.DatabaseAppRole == "" || c.DatabaseRunnerRole == "" || c.DatabaseAppRole == c.DatabaseRunnerRole {
		return errors.New("config: KRITIK_DATABASE_APP_ROLE and KRITIK_DATABASE_RUNNER_ROLE must be set and distinct")
	}
	if c.LeaderRetryInterval <= 0 {
		return fmt.Errorf("config: KRITIK_LEADER_RETRY_INTERVAL must be positive, got %s", c.LeaderRetryInterval)
	}
	switch c.Executor {
	case ExecutorKubernetes, ExecutorLocal:
	default:
		return fmt.Errorf("config: KRITIK_EXECUTOR must be kubernetes or local, got %q", c.Executor)
	}
	return c.validateWork()
}

// validateWork checks the settings that size the queues and the runners.
func (c *Config) validateWork() error {
	if c.ReviewWorkers <= 0 || c.IndexWorkers <= 0 {
		return errors.New("config: KRITIK_REVIEW_WORKERS and KRITIK_INDEX_WORKERS must be positive")
	}
	if c.RunnerTTL <= 0 {
		return errors.New("config: KRITIK_RUNNER_TTL must be positive")
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

// EnvVar is one environment variable Config reads, as this process has it.
// A secret, one the process unsets once read, shows only whether it is
// set. Set says whether the environment gave it, rather than its default.
type EnvVar struct {
	Name   string
	Value  string
	Secret bool
	Set    bool
}

// Env lists the variables Config reads, in the order it declares them. The
// environment has lost its secrets by the time Load returns, so a secret
// is set when its field holds a value.
func (c *Config) Env() []EnvVar {
	v := reflect.ValueOf(c).Elem()
	var out []EnvVar
	for i := range v.NumField() {
		name, opts, _ := strings.Cut(v.Type().Field(i).Tag.Get("env"), ",")
		if name == "" {
			continue
		}
		e := EnvVar{Name: name, Secret: slices.Contains(strings.Split(opts, ","), "unset")}
		field := v.Field(i)
		if e.Secret {
			e.Set, e.Value = !field.IsZero(), "not set"
			if e.Set {
				e.Value = "set"
			}
		} else {
			_, e.Set = os.LookupEnv(name)
			e.Value = fmt.Sprint(field.Interface())
		}
		out = append(out, e)
	}
	return out
}
