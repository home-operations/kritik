// Command kritik reviews GitHub pull requests against an index of the
// repository. "kritik serve" runs the service, and "kritik run" one review
// or index run in a runner Job the service creates (ADR-0024).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	// Blank import: its init() sets GOMEMLIMIT to 90% of the container's cgroup
	// memory limit (honoring an explicit GOMEMLIMIT / AUTOMEMLIMIT=off). The GC
	// is otherwise unaware of the cgroup limit, so a parse of a large repository
	// could OOM-kill the pod before the GC reclaims.
	_ "github.com/KimMachineGun/automemlimit"
	"golang.org/x/sync/errgroup"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/egress"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/poller"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/server"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/web"
	"github.com/home-operations/kritik/internal/webapi"
	"github.com/home-operations/kritik/internal/worker"
)

// Build metadata, set via -ldflags at release time (see Dockerfile / release.yaml).
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("kritik exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	command, err := config.ParseCommand(os.Args[1:])
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	runSpec, err := validate(command, cfg)
	if err != nil {
		return err
	}

	logger, err := newLogger(cfg)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	logger.Info("starting kritik",
		"version", version,
		"commit", commit,
		"command", command,
		"addr", cfg.Addr,
		"metrics_addr", cfg.MetricsAddr,
		"gateway_addr", cfg.GatewayAddr,
		"gateway_url", cfg.GatewayURL,
		"config_file", cfg.ConfigFile,
		"owner_dsn", cfg.DatabaseOwnerURL != "",
	)

	// Graceful shutdown on the usual termination signals. stop() runs as soon
	// as the first signal arrives so a second signal restores default handling
	// and force-terminates instead of being swallowed during a slow drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	// The management listener comes up first so liveness answers while the
	// database is still starting; readiness stays false until the store is
	// open, so the pod waits rather than being killed by its own probe.
	mgmt := server.NewManagement(cfg.MetricsAddr, logger)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return mgmt.Run(ctx) })

	// Both commands connect with the application DSN and refuse to start if
	// it could bypass row-level security or the vector extension is
	// missing; serve also opens the owner DSN, to lead.
	st, err := openStore(ctx, storeOptions(command, cfg, logger), logger)
	if err != nil {
		return err
	}
	defer st.Close()

	if command == config.CommandRun {
		// A runner does one thing and exits; it never becomes ready.
		return runner.Run(ctx, st, runSpec, runner.Secrets{GitToken: cfg.GitToken, GatewayToken: cfg.GatewayToken}, logger)
	}
	if err := serve(ctx, g, st, cfg, mgmt.Registry(), logger); err != nil {
		return err
	}
	mgmt.SetReady(true)

	if err := g.Wait(); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

// serve starts the service on g (ADR-0024): the configuration read at
// startup, the leader duties on the replica holding the leader lock, the
// webhooks, the dashboard, the gateway and the job queues.
func serve(
	ctx context.Context, g *errgroup.Group, st *store.Store, cfg *config.Config, reg *prometheus.Registry, logger *slog.Logger,
) error {
	drift := server.NewConfigDriftGauge(reg)
	configErrors := server.NewConfigErrorGauge(reg)
	m := metrics.New(reg)
	file, err := loadConfig(cfg.ConfigFile)
	if err != nil {
		return err
	}
	logConfig(logger, file, "configuration loaded")
	// Once read, a secret's variable is dropped, so no later lookup or
	// child process sees it (ADR-0022 §2.2).
	for _, name := range file.SecretEnv() {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("unset %s: %w", name, err)
		}
	}
	// current is the configuration read at startup; the leader applies it
	// on election, the other replica only compares hashes.
	current := configfile.NewCurrent(file)
	g.Go(func() error {
		return reportDrift(ctx, st, current, drift, driftInterval)
	})
	exec, err := newExecutor(ctx, cfg, logger)
	if err != nil {
		return err
	}
	// Insert-only client: the webhooks, the dashboard and the leader
	// enqueue jobs with it; startWorker's own client works the queues.
	inserter, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{Logger: logger})
	if err != nil {
		return fmt.Errorf("river: %w", err)
	}
	// One forge client per App and account, shared by the workers and the
	// leader's poll, so an installation's token is minted once.
	forges := &worker.ForgeCache{Build: worker.BuildForge}
	svc := ingest.NewService(st, inserter)
	if st.LeaderEligible() {
		sweeper, _ := exec.(*executor.Kube)
		g.Go(func() error {
			return st.RunAsLeader(ctx, cfg.LeaderRetryInterval, func(ctx context.Context) error {
				m.Leading(true)
				defer m.Leading(false)
				return lead(ctx, st, cfg, current, inserter, svc, forges, sweeper, m, configErrors, logger)
			})
		})
	} else {
		logger.Warn("no owner DSN configured; this replica can never migrate or apply configuration")
	}
	if err := startPublic(ctx, g, st, cfg, current, inserter, svc, m, logger); err != nil {
		return err
	}
	return startWorker(ctx, g, st, cfg, current, exec, forges, m, logger)
}

// errNoSignIn is a configuration that leaves the dashboard no way to sign
// in, which serve refuses to start with.
var errNoSignIn = errors.New("the dashboard has no way to sign in: set KRITIK_AUTH_ADMIN_PASSWORD, or auth in the configuration file")

// loadConfig reads the configuration file at path, none when path is "",
// for serve: it is read once, and a change takes a restart (ADR-0022
// §2.1).
func loadConfig(path string) (*configfile.File, error) {
	f, err := configfile.Load(path)
	if err != nil {
		return nil, err
	}
	if !f.Auth.Configured() {
		return nil, errNoSignIn
	}
	return f, nil
}

// startPublic serves the one public listener on Addr until ctx ends
// (ADR-0024 §2.2): the webhooks, and the dashboard with its sign-in and
// API.
func startPublic(
	ctx context.Context, g *errgroup.Group, st *store.Store, cfg *config.Config, current *configfile.Current,
	inserter *river.Client[pgx.Tx], svc *ingest.Service, m *metrics.Metrics, logger *slog.Logger,
) error {
	hooks := ingest.NewHandler(current, svc, logger.With("listener", "hooks"))
	hooks.Metrics = m
	hooks.Deliveries = svc

	webLogger := logger.With("listener", "web")
	authHandler, err := auth.New(auth.Config{Store: st, Current: current, WebURL: cfg.WebURLParsed(), Logger: webLogger})
	if err != nil {
		return err
	}
	api := webapi.New(webapi.Config{
		Store: st, Current: current, Auth: authHandler, UI: web.FS(),
		WebURL: cfg.WebURLParsed(), Version: version, Logger: webLogger, Actions: webapi.JobActions{Queue: inserter}, Env: cfg.Env(),
	})
	lctx := lingering(ctx, linger)
	g.Go(func() error { return api.Run(lctx) })
	public := server.Public(cfg.WebBasePath(), hooks, api.Handler())
	g.Go(func() error {
		return server.Serve(lctx, cfg.Addr, public, publicDrain, logger.With("listener", "public"))
	})
	return nil
}

// startWorker serves the gateway and works the job queues until ctx ends.
func startWorker(
	ctx context.Context, g *errgroup.Group, st *store.Store, cfg *config.Config, current *configfile.Current, exec executor.Executor,
	forges *worker.ForgeCache, m *metrics.Metrics, logger *slog.Logger,
) error {
	embedders := &worker.Embedders{Build: worker.BuildEmbedder}
	workers := river.NewWorkers()
	base := worker.Base{Store: st, Current: current, Forges: forges, Logger: logger, Metrics: m}
	completers := &worker.Completers{Build: worker.BuildStepper}
	// The gateway: runner pods' one route out, allowed by the hosts the
	// current configuration names (ADR-0008), and the model and similar-code
	// endpoints an agentic runner calls with its run token (ADR-0004,
	// ADR-0026).
	gatewayLogger := logger.With("listener", "gateway")
	gateway := &worker.Gateway{
		Store: st, Current: current, Logger: gatewayLogger, Metrics: m,
		Proxy: &egress.Proxy{
			Rules: current.Get().EgressRules, Observe: m.Egress, Logger: gatewayLogger,
		},
		Steppers: completers, Embedders: embedders,
	}
	g.Go(func() error {
		return server.Serve(lingering(ctx, linger), cfg.GatewayAddr, gateway, worker.GatewayDrain, gatewayLogger)
	})
	river.AddWorker(workers, &worker.Review{
		Base: base, Executor: exec, GatewayURL: cfg.GatewayURL, GatewayTokenTTL: cfg.GatewayTokenTTL,
	})
	river.AddWorker(workers, &worker.FollowUp{Base: base, Completers: completers})
	river.AddWorker(workers, &worker.Index{
		Base: base, Executor: exec, Embedders: embedders,
	})
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{
		Logger: logger,
		// Review and index workers set their own timeouts from the
		// runner deadline; rescue must wait out the longest of them.
		RescueStuckJobsAfter: jobtimeout.RescueStuckJobsAfter,
		// ctx ending starts a soft stop: running jobs get this long
		// before their contexts end (ADR-0024 §2.3).
		SoftStopTimeout: queueDrain,
		Queues: map[string]river.QueueConfig{
			jobs.QueueReview:   {MaxWorkers: cfg.ReviewWorkers},
			jobs.QueueFollowUp: {MaxWorkers: cfg.ReviewWorkers},
			jobs.QueueIndex:    {MaxWorkers: cfg.IndexWorkers},
		},
		Workers: workers,
	})
	if err != nil {
		return fmt.Errorf("river: %w", err)
	}
	g.Go(func() error { return workQueues(ctx, st, queue, cfg, logger) })
	return nil
}

// queueDrain is how long a stopping serve lets running jobs finish; a
// review still running then is cut and retried (ADR-0024 §2.3).
// queueStopHeadroom covers what a cut review does before it hands its job
// back, waiting for its agent row above all. Both fit in the chart's
// 150s termination grace period.
const (
	queueDrain        = 100 * time.Second
	queueStopHeadroom = 40 * time.Second
)

// validate checks what command needs of cfg beyond the common set, and
// reads a runner's spec.
func validate(command config.Command, cfg *config.Config) (runner.Spec, error) {
	if command == config.CommandServe {
		return runner.Spec{}, cfg.ValidateServe()
	}
	if err := cfg.ValidateRunner(); err != nil {
		return runner.Spec{}, err
	}
	return runner.ReadSpec(cfg.RunSpecFile)
}

// storeOptions is how command connects to the database: serve with the
// owner DSN too, which leading needs, a runner never.
func storeOptions(command config.Command, cfg *config.Config, logger *slog.Logger) store.Options {
	opts := store.Options{
		AppURL: cfg.DatabaseURL, Logger: logger,
	}
	if command == config.CommandServe {
		opts.OwnerURL = cfg.DatabaseOwnerURL
	}
	return opts
}

// linger is how long the public listener and the gateway keep accepting
// connections once serve is told to stop: Kubernetes and the proxies in
// front of it take a moment to stop routing to a terminating pod, and a
// connection refused meanwhile is a webhook lost, since GitHub does not
// redeliver on its own, or a runner's model step retried.
const linger = 5 * time.Second

// lingering is a context that ends d after ctx does, with ctx's values.
func lingering(ctx context.Context, d time.Duration) context.Context {
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		<-ctx.Done()
		t := time.NewTimer(d)
		defer t.Stop()
		<-t.C
		cancel()
	}()
	return lctx
}

// publicDrain is how long a stopping serve lets webhook and dashboard
// requests finish. Event streams end at once, when the API's Run returns.
const publicDrain = 10 * time.Second

// workQueues works the job queues until ctx ends, then waits for River's
// soft stop to let running jobs finish or cut them. The queue's tables come
// from migrations, which the leader runs; on a fresh database that may be
// this very process a moment from now, or another replica, so it waits for
// the schema rather than racing it.
func workQueues(ctx context.Context, st *store.Store, queue *river.Client[pgx.Tx], cfg *config.Config, logger *slog.Logger) error {
	if err := st.WaitForSchema(ctx, 2*time.Second); err != nil {
		return nil // shutdown while waiting
	}
	if err := startQueue(ctx, queue, logger); err != nil {
		return err
	}
	logger.Info("working the queues", "review_workers", cfg.ReviewWorkers, "index_workers", cfg.IndexWorkers, "executor", cfg.Executor)
	<-ctx.Done()
	select {
	case <-queue.Stopped():
	case <-time.After(queueDrain + queueStopHeadroom):
		logger.Warn("the queues did not stop in time", "drain", queueDrain)
	}
	return nil
}

func startQueue(ctx context.Context, queue *river.Client[pgx.Tx], logger *slog.Logger) error {
	const retry = 5 * time.Second
	const attempts = 24
	var err error
	for i := 1; i <= attempts; i++ {
		if err = queue.Start(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		logger.Warn("queue start failed, retrying", "error", err, "attempt", i, "after", retry)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
	return fmt.Errorf("river: start: %w", err)
}

// newExecutor builds the runner executor the configuration selects.
func newExecutor(ctx context.Context, cfg *config.Config, logger *slog.Logger) (executor.Executor, error) {
	if cfg.Executor == config.ExecutorLocal {
		runnerStore, err := store.Open(ctx, store.Options{AppURL: cfg.RunnerDatabaseURL, Logger: logger})
		if err != nil {
			return nil, err
		}
		return &executor.Local{Store: runnerStore}, nil
	}
	client, ns, err := executor.NewKubeInCluster()
	if err != nil {
		return nil, err
	}
	return &executor.Kube{
		Client: client, Namespace: ns, Image: cfg.RunnerImage, ServiceAccount: cfg.RunnerServiceAccount,
		DatabaseSecret: cfg.RunnerDatabaseSecret, DatabaseSecretKey: cfg.RunnerDatabaseSecretKey,
		GatewayURL: cfg.GatewayURL, RuntimeClass: cfg.RunnerRuntimeClass, TTL: cfg.RunnerTTL, Logger: logger,
	}, nil
}

// openStore retries until the database answers, because in a fresh
// deployment Postgres is usually still bootstrapping when the pod starts.
// A configuration error (a DSN that would bypass row-level security, no
// vector extension) is returned at once: waiting would not change it.
func openStore(ctx context.Context, opts store.Options, logger *slog.Logger) (*store.Store, error) {
	const retry = 5 * time.Second
	for {
		st, err := store.Open(ctx, opts)
		if err == nil {
			return st, nil
		}
		if store.IsConfigurationError(err) || ctx.Err() != nil {
			return nil, err
		}
		logger.Warn("database not ready, retrying", "error", err, "after", retry)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

// secretSweepInterval is how often the leader deletes the Secrets of runs
// that no longer need one.
const secretSweepInterval = 5 * time.Minute

// lead runs for as long as this replica holds the leader lock: migrate,
// apply the configuration and register the repositories its Apps reach,
// then keep the backstop poll, the Secret sweep, onboarding and retention
// going. While the configuration has an embedder it also keeps the index
// schema to it and enqueues an onboarding index job for every repository
// that has none.
func lead(
	ctx context.Context, st *store.Store, cfg *config.Config, current *configfile.Current, queue *river.Client[pgx.Tx],
	svc *ingest.Service, forges *worker.ForgeCache, sweeper *executor.Kube, m *metrics.Metrics, configErrors *server.ConfigErrorGauge,
	logger *slog.Logger,
) error {
	if err := st.Migrate(ctx, cfg.DatabaseAppRole, cfg.DatabaseRunnerRole); err != nil {
		return err
	}
	// Every leader duty ends before lead returns and the lock is released,
	// so the next leader never runs one alongside this replica's.
	pollCtx, stopPoll := context.WithCancel(ctx)
	var duties sync.WaitGroup
	defer duties.Wait()
	defer stopPoll()
	// The backstop poll is a leader duty: one lister per connection.
	poll := &poller.Poller{
		Store: st, Current: current, Forges: forges, Reach: worker.ReachRepositories, Dispatcher: svc, Logger: logger, Metrics: m,
	}
	duties.Go(func() { poll.Run(pollCtx) })
	// So is deleting, by name, run Secrets a dead worker left without an
	// owner. Like the poller it walks the configured accounts, each under
	// its own row-level security scope.
	if sweeper != nil {
		duties.Go(func() {
			sweeper.RunSecretSweeper(pollCtx, st, func() []string {
				accounts := current.Get().Accounts
				ids := make([]string, 0, len(accounts))
				for i := range accounts {
					ids = append(ids, accounts[i].ID())
				}
				return ids
			}, secretSweepInterval)
		})
	}
	// So is feeding the index queue its onboarding jobs, a few at a time.
	onboarder := &worker.Onboarder{Store: st, Queue: queue, Current: current, Logger: logger}
	duties.Go(func() { onboarder.Run(pollCtx) })
	// And so is retention: model-call transcripts past their configured
	// window and the indexes of repositories disabled past their grace
	// (owner pool, bypassing row-level security), and expired dashboard
	// sessions (app pool).
	duties.Go(func() { retentionSweep(pollCtx, st, current, retentionSweepInterval, logger) })
	if err := applyConfig(ctx, current.Get(), func(ctx context.Context, f *configfile.File) error {
		if err := st.ApplyConfig(ctx, f); err != nil {
			return err
		}
		if err := ensureIndexSchema(ctx, st, cfg.DatabaseAppRole, f.Embedding, logger); err != nil {
			return err
		}
		// Once the accounts exist: a fresh instance, or a new connection,
		// knows its repositories before any webhook names one.
		poll.SyncRepositories(ctx)
		return nil
	}, onboarder.Offer, configErrors, logger); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// ensureIndexSchema keeps the index table to the configuration's embedder,
// rebuilding it, and so every repository's index, when the model or
// dimension changed: the dashboard asked the admin to confirm that before
// saving it, and a configuration file edit makes it without asking.
// Without an embedder the table is left as it is.
func ensureIndexSchema(ctx context.Context, st *store.Store, appRole string, e *configfile.Embedding, logger *slog.Logger) error {
	if e == nil {
		return nil
	}
	rebuilt, err := st.EnsureIndexSchema(ctx, appRole, e.Model, e.Dims)
	if rebuilt {
		logger.Warn("index rebuilt for a new embedder: every repository is indexed again", "model", e.Model, "dims", e.Dims)
	}
	return err
}

// retentionSweepInterval is how often the leader deletes model-call
// transcripts, review diffs, disabled repositories' indexes and dashboard
// sessions past their retention window.
const retentionSweepInterval = time.Hour

// retentionStore is the subset of *store.Store that retentionSweep needs,
// narrowed so it can be exercised in tests with a fake.
type retentionStore interface {
	SweepModelCalls(ctx context.Context, olderThan time.Duration) (int64, error)
	SweepDiffs(ctx context.Context, olderThan time.Duration) (int64, error)
	SweepSessions(ctx context.Context, now time.Time) (int64, error)
	SweepDisabledIndexes(ctx context.Context, grace time.Duration) (int64, error)
}

// retentionSweep runs once immediately, then every interval until ctx ends,
// deleting model-call transcripts older than the current configuration's
// retention window and the indexes of repositories disabled for longer than its
// disabledIndexGrace (owner pool, bypassing row-level security), and
// expired dashboard sessions (app pool). A sweep failure is logged, never
// fatal: it just leaves stale rows for the next tick.
func retentionSweep(ctx context.Context, st retentionStore, current *configfile.Current, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := st.SweepModelCalls(ctx, current.Get().TranscriptRetention()); err != nil {
			if ctx.Err() == nil {
				logger.Warn("model call transcripts not swept", "error", err)
			}
		} else if n > 0 {
			logger.Info("model call transcripts swept", "rows", n)
		}
		if n, err := st.SweepDiffs(ctx, current.Get().DiffRetention()); err != nil {
			if ctx.Err() == nil {
				logger.Warn("review diffs not swept", "error", err)
			}
		} else if n > 0 {
			logger.Info("review diffs swept", "packs", n)
		}
		if n, err := st.SweepSessions(ctx, time.Now()); err != nil {
			if ctx.Err() == nil {
				logger.Warn("dashboard sessions not swept", "error", err)
			}
		} else if n > 0 {
			logger.Info("dashboard sessions swept", "rows", n)
		}
		if n, err := st.SweepDisabledIndexes(ctx, current.Get().DisabledIndexGrace()); err != nil {
			if ctx.Err() == nil {
				logger.Warn("disabled repositories' indexes not swept", "error", err)
			}
		} else if n > 0 {
			logger.Info("disabled repositories' indexes swept", "repositories", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// applyConfig applies f to the store once, on election, and calls
// onApplied on success. A configuration the store refuses for its content
// (store.IsConfigContentError) must not end leadership, or every replica
// would crash-loop on it in turn: it is logged and raised on the gauge,
// the last applied state stays, and the same content would be refused
// again, so it is not retried before a restart. Any other error is
// returned, which ends the process for a restart.
func applyConfig(
	ctx context.Context, f *configfile.File, apply func(context.Context, *configfile.File) error,
	onApplied func(context.Context) error, gauge *server.ConfigErrorGauge, logger *slog.Logger,
) error {
	h := f.Hash()
	err := apply(ctx, f)
	switch {
	case err != nil && store.IsConfigContentError(err):
		gauge.Set(true)
		logger.Error("configuration refused by the store, keeping the last applied one", "hash", h[:12], "error", err)
		return nil
	case err != nil:
		return err
	}
	gauge.Set(false)
	logger.Info("configuration applied to the store", "hash", h[:12])
	return onApplied(ctx)
}

// driftInterval is how often a replica compares its configuration with
// the one the leader applied, which differ while a rollout is part done.
const driftInterval = 10 * time.Second

// reportDrift compares this replica's file with what the leader applied and
// exposes a mismatch as a gauge. It is deliberately not on /readyz: a stale
// ConfigMap on one node must not take an ingest replica out of the Service.
func reportDrift(
	ctx context.Context, st *store.Store, current *configfile.Current, gauge *server.ConfigDriftGauge, every time.Duration,
) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			applied, err := st.AppliedConfigHash(ctx)
			if err != nil {
				slog.Warn("could not read applied configuration hash", "error", err)
				continue
			}
			gauge.Set(applied != "" && applied != current.Get().Hash())
		}
	}
}

func logConfig(logger *slog.Logger, f *configfile.File, msg string) {
	logger.Info(msg, "providers", len(f.Providers), "accounts", len(f.Accounts), "connections", len(f.Connections))
}

func newLogger(cfg *config.Config) (*slog.Logger, error) {
	level, err := cfg.Level()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.LogFormat, "text") {
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
}
