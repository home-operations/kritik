// Package config loads kritika's process configuration from environment
// variables: what the process itself needs to start. What kritika reviews
// and how lives in the configuration file and its KRITIKA_* overlay
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

	"github.com/home-operations/kritika/internal/egress"
	"github.com/home-operations/kritika/internal/store"
)

// Command selects what a kritika process runs: the service, or one review or
// index run in a runner Job.
type Command string

// Commands a kritika process can run.
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
	return "", fmt.Errorf("config: unknown arguments %q: usage: kritika [serve | run]", args)
}

// Executor selects how runners run.
type Executor string

// Executors a kritika process can run runners with.
const (
	// ExecutorKubernetes creates a Job per run in the pod's own namespace.
	ExecutorKubernetes Executor = "kubernetes"
	// ExecutorLocal runs the runner in-process and is for development and
	// tests only, because it gives the checkout the worker's credentials.
	ExecutorLocal Executor = "local"
)

// Config holds the process configuration for kritika. All fields are populated
// from environment variables via caarlos0/env. Call [Load] to parse and
// validate; do not construct directly.
type Config struct {
	// Addr is the listen address of serve's one public listener: the
	// webhooks, /hooks/{connection}, and the dashboard. Port 8080 matches
	// the container image's EXPOSE and the other services in the fleet.
	Addr string `env:"KRITIKA_ADDR" envDefault:":8080"`

	// MetricsAddr is the listen address for /healthz, /readyz and /metrics.
	// Kept on a separate port from the public listener, as konflate does, so
	// the management endpoints are never reachable through the ingress that
	// fronts it.
	MetricsAddr string `env:"KRITIKA_METRICS_ADDR" envDefault:":8081"`

	// GatewayAddr is the listen address of the gateway serve runs: the
	// forward proxy runner pods reach the outside through, and the model
	// and similar-code endpoints a runner calls with its run token. Its own
	// port, so the runner network policy can name it without opening the
	// public or management listeners.
	GatewayAddr string `env:"KRITIKA_GATEWAY_ADDR" envDefault:":8082"`

	// GatewayURL is the gateway's in-cluster address, http://host:port:
	// runner Jobs are handed it as HTTPS_PROXY and HTTP_PROXY, and a
	// review's job document names it as its model endpoint. serve needs it,
	// since every review reaches its model through it.
	GatewayURL string `env:"KRITIKA_GATEWAY_URL"`

	// GatewayTokenTTL is how long a run token outlives its runner Job's
	// deadline before it expires on its own, in case the worker that
	// minted it dies before revoking it.
	GatewayTokenTTL time.Duration `env:"KRITIKA_GATEWAY_TOKEN_TTL" envDefault:"1h"`

	// WebURL is the dashboard's externally reachable origin: an absolute
	// http(s) URL with a host and no query or fragment. It is how the
	// dashboard builds absolute links (OIDC redirect URIs, session cookie
	// scope) back to itself, so it must match how the ingress/HTTPRoute
	// actually exposes it. A trailing slash is trimmed. Required to serve.
	// Parsed once into an unexported *url.URL, read back with
	// [Config.WebURLParsed].
	WebURL string `env:"KRITIKA_WEB_URL"`

	// ConfigFile is the path of the optional configuration file: sign-in
	// and the connections an admin keeps in git. serve loads it at startup,
	// and a change takes a restart. Empty means no file, and the
	// environment alone declares them.
	ConfigFile string `env:"KRITIKA_CONFIG_FILE"`

	// The database. Every process connects as its own role for request and
	// job work: serve as the application role, which must not be a
	// superuser, have BYPASSRLS or own anything, so row-level security
	// applies to it, and a runner pod as the runner role, under the same
	// variables. Checked at startup; a role that bypasses row-level
	// security refuses to start. The connection is DatabaseURL, a
	// connection URI, or is built from the parameters: DatabaseHost,
	// DatabasePort, DatabaseName, DatabaseSSLMode and
	// DatabaseConnectTimeout, which every role shares, with the role's
	// DatabaseUser and DatabasePassword. Built, the password needs no URI
	// escaping, so a role's Secret can be the plain username and password
	// its operator writes.
	DatabaseURL  string `env:"KRITIKA_DATABASE_URL,unset"`
	DatabaseHost string `env:"KRITIKA_DATABASE_HOST"`
	// DatabasePort defaults to Postgres's own.
	DatabasePort uint16 `env:"KRITIKA_DATABASE_PORT" envDefault:"5432"`
	DatabaseName string `env:"KRITIKA_DATABASE_NAME" envDefault:"kritika"`
	// DatabaseSSLMode is libpq's sslmode. require, the default, encrypts
	// without verifying the server's certificate, which is what an
	// in-cluster Postgres with an operator-issued certificate offers
	// without more setup.
	DatabaseSSLMode string `env:"KRITIKA_DATABASE_SSLMODE" envDefault:"require"`
	// DatabaseConnectTimeout bounds one connection attempt. Without it a
	// dial to a Service address DNS still caches after a redeploy hangs for
	// minutes before kritika retries; with it the retry looks the name up
	// again.
	DatabaseConnectTimeout time.Duration `env:"KRITIKA_DATABASE_CONNECT_TIMEOUT" envDefault:"10s"`
	DatabaseUser           string        `env:"KRITIKA_DATABASE_USER"`
	DatabasePassword       string        `env:"KRITIKA_DATABASE_PASSWORD,unset"`

	// The owner role owns the schema and runs migrations, the configuration
	// loader and the chunks DDL, all of which write across accounts. serve
	// needs it to become leader; a runner must not have it. Its connection
	// is DatabaseOwnerURL, or is built from DatabaseOwnerUser and
	// DatabaseOwnerPassword with the shared parameters.
	DatabaseOwnerURL      string `env:"KRITIKA_DATABASE_OWNER_URL,unset"`
	DatabaseOwnerUser     string `env:"KRITIKA_DATABASE_OWNER_USER"`
	DatabaseOwnerPassword string `env:"KRITIKA_DATABASE_OWNER_PASSWORD,unset"`

	// DatabaseAppRole and DatabaseRunnerRole are the Postgres role names the
	// owner grants privileges to during migrations. They default to what
	// deploy/dev and the chart's CloudNativePG example declare.
	DatabaseAppRole    string `env:"KRITIKA_DATABASE_APP_ROLE" envDefault:"kritika_app"`
	DatabaseRunnerRole string `env:"KRITIKA_DATABASE_RUNNER_ROLE" envDefault:"kritika_runner"`

	// LeaderRetryInterval is how often a leader-eligible replica retries the
	// leader lock and how often the holder verifies it still has it.
	LeaderRetryInterval time.Duration `env:"KRITIKA_LEADER_RETRY_INTERVAL" envDefault:"15s"`

	// ReviewWorkers is how many review jobs one worker replica runs at once.
	// Each one holds a runner pod open for the length of a fetch and diff,
	// so this bounds pods per replica, not model calls.
	ReviewWorkers int `env:"KRITIKA_REVIEW_WORKERS" envDefault:"2"`
	// IndexWorkers is how many index jobs one worker replica runs at once;
	// indexing is rate-limited apart from reviews so onboarding a large
	// account cannot starve them. Each holds a runner pod too, so a replica
	// runs at most ReviewWorkers + IndexWorkers runner pods.
	IndexWorkers int `env:"KRITIKA_INDEX_WORKERS" envDefault:"1"`

	// Executor selects how runners run; see the Executor type.
	Executor Executor `env:"KRITIKA_EXECUTOR" envDefault:"kubernetes"`

	// RunnerImage is the image runner Jobs use, normally serve's own.
	// Required to serve with the kubernetes executor.
	RunnerImage string `env:"KRITIKA_RUNNER_IMAGE"`

	// RunnerServiceAccount is the permissionless service account runner pods
	// run as.
	RunnerServiceAccount string `env:"KRITIKA_RUNNER_SERVICE_ACCOUNT" envDefault:"kritika-runner"`
	// RunnerDatabaseSecret is the Secret a runner Job reads the runner
	// role's credentials from: the username and password under
	// RunnerDatabaseSecretUserKey and RunnerDatabaseSecretPasswordKey, a
	// basic-auth Secret's keys, which the Job pairs with serve's own
	// database parameters, or, when RunnerDatabaseSecretKey is set, a
	// connection URI under that key.
	RunnerDatabaseSecret            string `env:"KRITIKA_RUNNER_DATABASE_SECRET" envDefault:"kritika-postgres-runner"`
	RunnerDatabaseSecretKey         string `env:"KRITIKA_RUNNER_DATABASE_SECRET_KEY"`
	RunnerDatabaseSecretUserKey     string `env:"KRITIKA_RUNNER_DATABASE_SECRET_USER_KEY" envDefault:"username"`
	RunnerDatabaseSecretPasswordKey string `env:"KRITIKA_RUNNER_DATABASE_SECRET_PASSWORD_KEY" envDefault:"password"`

	// RunnerTTL is how long a finished Job stays for kubectl before
	// Kubernetes removes it. The run row keeps everything the Job knew.
	RunnerTTL time.Duration `env:"KRITIKA_RUNNER_TTL" envDefault:"10m"`

	// RunnerRuntimeClass is the RuntimeClass runner pods run under, such as
	// a gVisor or Kata class, so a pod that parses untrusted repository
	// content is kept from the node's kernel. Empty uses the cluster's
	// default runtime. Advised, not required.
	RunnerRuntimeClass string `env:"KRITIKA_RUNNER_RUNTIME_CLASS"`

	// The runner role's own connection, needed only by the local executor,
	// which runs the runner inside the serve process: RunnerDatabaseURL,
	// or RunnerDatabaseUser and RunnerDatabasePassword with the shared
	// parameters.
	RunnerDatabaseURL      string `env:"KRITIKA_RUNNER_DATABASE_URL,unset"`
	RunnerDatabaseUser     string `env:"KRITIKA_RUNNER_DATABASE_USER"`
	RunnerDatabasePassword string `env:"KRITIKA_RUNNER_DATABASE_PASSWORD,unset"`

	// RunSpecFile is the path of a runner's job document, a
	// versioned JSON runner spec the worker mounts into the Job from the
	// run's Secret. A file rather than a variable: a spec can outgrow the
	// kernel's 128 KiB limit on one environment string.
	RunSpecFile string `env:"KRITIKA_RUN_SPEC_FILE"`
	// GitToken and GatewayToken are the runner's credentials, read from
	// the run's own Secret. The gateway token is set only for an agentic
	// review.
	GitToken     string `env:"KRITIKA_GIT_TOKEN,unset"`
	GatewayToken string `env:"KRITIKA_GATEWAY_TOKEN,unset"`

	// LogLevel is the minimum slog level emitted: debug, info, warn or error.
	LogLevel string `env:"KRITIKA_LOG_LEVEL" envDefault:"info"`

	// LogFormat selects the slog handler: "json" (the default, for containers)
	// or "text" for local runs.
	LogFormat string `env:"KRITIKA_LOG_FORMAT" envDefault:"json"`

	webURL *url.URL
}

// ValidateServe checks what serve needs beyond the common set.
func (c *Config) ValidateServe() error {
	var errs []error
	if c.Executor == ExecutorKubernetes && c.RunnerImage == "" {
		errs = append(errs, errors.New("config: KRITIKA_RUNNER_IMAGE is required with the kubernetes executor"))
	}
	if c.Executor == ExecutorKubernetes && c.RunnerDatabaseSecretKey == "" && c.DatabaseHost == "" {
		errs = append(errs, errors.New("config: KRITIKA_DATABASE_HOST is required for runner Jobs to connect with the username and password "+
			"in KRITIKA_RUNNER_DATABASE_SECRET; or KRITIKA_RUNNER_DATABASE_SECRET_KEY names the key of a connection URI"))
	}
	if c.Executor == ExecutorLocal && !c.RunnerDatabase().Set() {
		errs = append(errs, errors.New("config: KRITIKA_RUNNER_DATABASE_URL, or KRITIKA_RUNNER_DATABASE_USER and "+
			"KRITIKA_RUNNER_DATABASE_PASSWORD, is required with the local executor"))
	}
	if c.WebURL == "" {
		errs = append(errs, errors.New("config: KRITIKA_WEB_URL is required to serve"))
	}
	if c.GatewayURL == "" {
		errs = append(errs, errors.New("config: KRITIKA_GATEWAY_URL is required to serve: every review reaches its model through the gateway"))
	}
	return errors.Join(errs...)
}

// ValidateRunner checks what a runner pod needs. The job document itself is
// decoded and validated by the runner package.
func (c *Config) ValidateRunner() error {
	if c.RunSpecFile == "" {
		return errors.New("config: KRITIKA_RUN_SPEC_FILE is required to run")
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
		return fmt.Errorf("config: KRITIKA_WEB_URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: KRITIKA_WEB_URL must be an absolute http(s) URL, got %q", c.WebURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config: KRITIKA_WEB_URL must not have a query or fragment, got %q", c.WebURL)
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
		return fmt.Errorf("config: KRITIKA_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	if c.GatewayURL != "" {
		if _, err := egress.ProxyURL(c.GatewayURL); err != nil {
			return fmt.Errorf("config: KRITIKA_GATEWAY_URL: %w", err)
		}
	}
	if c.GatewayTokenTTL <= 0 {
		return fmt.Errorf("config: KRITIKA_GATEWAY_TOKEN_TTL must be positive, got %s", c.GatewayTokenTTL)
	}
	if err := c.parseWebURL(); err != nil {
		return err
	}
	if err := c.validateDatabase(); err != nil {
		return err
	}
	if c.DatabaseAppRole == "" || c.DatabaseRunnerRole == "" || c.DatabaseAppRole == c.DatabaseRunnerRole {
		return errors.New("config: KRITIKA_DATABASE_APP_ROLE and KRITIKA_DATABASE_RUNNER_ROLE must be set and distinct")
	}
	if c.LeaderRetryInterval <= 0 {
		return fmt.Errorf("config: KRITIKA_LEADER_RETRY_INTERVAL must be positive, got %s", c.LeaderRetryInterval)
	}
	switch c.Executor {
	case ExecutorKubernetes, ExecutorLocal:
	default:
		return fmt.Errorf("config: KRITIKA_EXECUTOR must be kubernetes or local, got %q", c.Executor)
	}
	return c.validateWork()
}

// sslModes are libpq's sslmode values.
var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// validateDatabase checks that the process's own role has a connection,
// and that every role connecting by username has the shared parameters
// to connect with.
func (c *Config) validateDatabase() error {
	if c.DatabaseURL == "" && c.DatabaseUser == "" {
		return errors.New("config: KRITIKA_DATABASE_URL, or KRITIKA_DATABASE_HOST with KRITIKA_DATABASE_USER and KRITIKA_DATABASE_PASSWORD, " +
			"is required")
	}
	byUser := (c.DatabaseURL == "" && c.DatabaseUser != "") || (c.DatabaseOwnerURL == "" && c.DatabaseOwnerUser != "") ||
		(c.RunnerDatabaseURL == "" && c.RunnerDatabaseUser != "")
	if byUser && c.DatabaseHost == "" {
		return errors.New("config: KRITIKA_DATABASE_HOST is required to connect with a username and password")
	}
	if !slices.Contains(sslModes, c.DatabaseSSLMode) {
		return fmt.Errorf("config: KRITIKA_DATABASE_SSLMODE must be one of %s, got %q", strings.Join(sslModes, ", "), c.DatabaseSSLMode)
	}
	if c.DatabaseConnectTimeout <= 0 {
		return fmt.Errorf("config: KRITIKA_DATABASE_CONNECT_TIMEOUT must be positive, got %s", c.DatabaseConnectTimeout)
	}
	return nil
}

// DatabaseParams is the connection every role shares, without a role:
// host, port, database, sslmode and connect timeout.
func (c *Config) DatabaseParams() store.Conn {
	return store.Conn{
		Host: c.DatabaseHost, Port: c.DatabasePort, Database: c.DatabaseName, SSLMode: c.DatabaseSSLMode,
		ConnectTimeout: c.DatabaseConnectTimeout,
	}
}

// Database is the connection the process makes as its own role.
func (c *Config) Database() store.Conn {
	return c.conn(c.DatabaseURL, c.DatabaseUser, c.DatabasePassword)
}

// OwnerDatabase is the owner role's connection, unset when the process
// has none.
func (c *Config) OwnerDatabase() store.Conn {
	return c.conn(c.DatabaseOwnerURL, c.DatabaseOwnerUser, c.DatabaseOwnerPassword)
}

// RunnerDatabase is the runner role's connection for the local executor,
// unset when the process has none.
func (c *Config) RunnerDatabase() store.Conn {
	return c.conn(c.RunnerDatabaseURL, c.RunnerDatabaseUser, c.RunnerDatabasePassword)
}

// conn is one role's connection: its URL, or the shared parameters with
// its user and password, or unset when it has neither.
func (c *Config) conn(connString, user, password string) store.Conn {
	switch {
	case connString != "":
		return store.Conn{URL: connString}
	case user == "":
		return store.Conn{}
	}
	conn := c.DatabaseParams()
	conn.User, conn.Password = user, password
	return conn
}

// validateWork checks the settings that size the queues and the runners.
func (c *Config) validateWork() error {
	if c.ReviewWorkers <= 0 || c.IndexWorkers <= 0 {
		return errors.New("config: KRITIKA_REVIEW_WORKERS and KRITIKA_INDEX_WORKERS must be positive")
	}
	if c.RunnerTTL <= 0 {
		return errors.New("config: KRITIKA_RUNNER_TTL must be positive")
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
		return 0, fmt.Errorf("config: KRITIKA_LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
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
