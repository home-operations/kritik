// Command kritik reviews GitHub pull requests against an index of the
// repository. One binary serves every role; --role selects
// which part of the service this process runs.
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
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configsource"
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
	roleFlag := pflag.String("role", string(config.RoleAll), "process role: all, ingest, worker, runner or web")
	pflag.Parse()
	role, err := config.ParseRole(*roleFlag)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	runSpec, err := validateRole(role, cfg)
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
		"role", role,
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
	drift := server.NewConfigDriftGauge(mgmt.Registry())
	configErrors := server.NewConfigErrorGauge(mgmt.Registry())
	m := metrics.New(mgmt.Registry())
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return mgmt.Run(ctx) })

	// Every role connects with the application DSN and refuses to start if
	// that DSN could bypass row-level security or the vector extension is
	// missing. Leader-eligible roles also open the owner DSN.
	st, err := openStore(ctx, storeOptions(role, cfg, logger), logger)
	if err != nil {
		return err
	}
	defer st.Close()

	// The runner gets everything it needs from its Job spec; every other role
	// is driven by the configuration file, and must not start without it.
	var current *configfile.Current
	var exec executor.Executor
	if role != config.RoleRunner {
		// current is the configuration read at startup; the leader applies
		// it on election, followers only compare hashes.
		src := &configsource.Source{RequireSignIn: role == config.RoleAll || role == config.RoleWeb}
		file, err := src.Load(cfg.ConfigFile)
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
		current = src.Current
		g.Go(func() error {
			return reportDrift(ctx, st, current, drift, driftInterval)
		})
		if st.LeaderEligible() {
			// Only an all or worker role holds the owner DSN, so a leader
			// always has the executor it would sweep up after.
			exec, err = newExecutor(ctx, cfg, logger)
			if err != nil {
				return err
			}
			sweeper, _ := exec.(*executor.Kube)
			// Insert-only client: the leader enqueues onboarding index jobs.
			leaderQueue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{Logger: logger})
			if err != nil {
				return fmt.Errorf("river: %w", err)
			}
			g.Go(func() error {
				return st.RunAsLeader(ctx, cfg.LeaderRetryInterval, func(ctx context.Context) error {
					return lead(ctx, st, cfg, current, leaderQueue, sweeper, m, configErrors, logger)
				})
			})
		} else if role != config.RoleIngest && role != config.RoleWeb {
			logger.Warn("no owner DSN configured; this replica can never migrate or apply configuration")
		}
	}

	if role == config.RoleRunner {
		// A runner does one thing and exits; it never becomes ready.
		return runner.Run(ctx, st, runSpec, runner.Secrets{GitToken: cfg.GitToken, GatewayToken: cfg.GatewayToken}, logger)
	}

	if role == config.RoleAll || role == config.RoleIngest {
		// Insert-only River client: ingest enqueues, it never works jobs.
		queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{Logger: logger})
		if err != nil {
			return fmt.Errorf("river: %w", err)
		}
		svc := ingest.NewService(st, queue)
		handler := ingest.NewHandler(current, svc, logger)
		handler.Metrics = m
		handler.Deliveries = svc
		hooks := server.NewHooks(cfg.Addr, cfg.WebBasePath(), handler, logger)
		g.Go(func() error { return hooks.Run(ctx) })
	}
	if err := startWeb(ctx, g, role, st, cfg, current, logger); err != nil {
		return err
	}
	if role == config.RoleAll || role == config.RoleWorker {
		if exec == nil {
			if exec, err = newExecutor(ctx, cfg, logger); err != nil {
				return err
			}
		}
		embedders := &worker.Embedders{Build: worker.BuildEmbedder}
		forges := &worker.ForgeCache{Build: worker.BuildForge}
		workers := river.NewWorkers()
		base := worker.Base{Store: st, Current: current, Forges: forges, Logger: logger, Metrics: m}
		completers := &worker.Completers{Build: worker.BuildStepper}
		// The gateway: runner pods' one route out, allowed by the hosts the
		// current configuration names (ADR-0008), and the model endpoint an
		// agentic runner calls with its run token (ADR-0004).
		gatewayLogger := logger.With("listener", "gateway")
		gateway := &worker.Gateway{
			Store: st, Current: current, Logger: gatewayLogger, Metrics: m,
			Proxy: &egress.Proxy{
				Rules:   func() egress.Rules { return current.Get().EgressRules() },
				Observe: m.Egress, Logger: gatewayLogger,
			},
			Steppers: completers,
		}
		g.Go(func() error {
			return server.ServeDrain(ctx, cfg.GatewayAddr, gateway, worker.GatewayDrain, gatewayLogger)
		})
		river.AddWorker(workers, &worker.Review{
			Base: base, Executor: exec, Completers: completers, Embedders: embedders,
			GatewayURL: cfg.GatewayURL, GatewayTokenTTL: cfg.GatewayTokenTTL,
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
		// The queue's tables come from migrations, which the leader runs;
		// on a fresh database that may be this very process a moment from
		// now, or another replica. Wait for the schema rather than racing it.
		g.Go(func() error {
			if err := st.WaitForSchema(ctx, 2*time.Second); err != nil {
				return nil // shutdown while waiting
			}
			if err := startQueue(ctx, queue, logger); err != nil {
				return err
			}
			logger.Info("working the queues", "review_workers", cfg.ReviewWorkers, "index_workers", cfg.IndexWorkers, "executor", cfg.Executor)
			<-ctx.Done()
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return queue.Stop(stopCtx)
		})
	}
	mgmt.SetReady(true)

	if err := g.Wait(); err != nil {
		return fmt.Errorf("%s: %w", role, err)
	}
	return nil
}

// validateRole checks what role needs of cfg beyond the common set, and
// reads a runner's spec.
func validateRole(role config.Role, cfg *config.Config) (runner.Spec, error) {
	switch role {
	case config.RoleAll:
		return runner.Spec{}, errors.Join(cfg.ValidateWorker(), cfg.ValidateWeb())
	case config.RoleWorker:
		return runner.Spec{}, cfg.ValidateWorker()
	case config.RoleRunner:
		if err := cfg.ValidateRunner(); err != nil {
			return runner.Spec{}, err
		}
		return runner.ReadSpec(cfg.RunSpecFile)
	case config.RoleWeb:
		return runner.Spec{}, cfg.ValidateWeb()
	}
	return runner.Spec{}, nil
}

// storeOptions is how role connects to the database. Only a
// leader-eligible role, all or worker, is given the owner DSN; the web role
// in particular never is (ADR-0009 §3).
func storeOptions(role config.Role, cfg *config.Config, logger *slog.Logger) store.Options {
	opts := store.Options{
		AppURL: cfg.DatabaseURL, Logger: logger,
	}
	if role == config.RoleAll || role == config.RoleWorker {
		opts.OwnerURL = cfg.DatabaseOwnerURL
	}
	return opts
}

// webDrain is how long a stopping web role lets requests finish. Event
// streams end at once, when the API's Run returns.
const webDrain = 10 * time.Second

// startWeb serves the dashboard, its sign-in and its API on WebAddr until
// ctx ends, for the all and web roles.
func startWeb(
	ctx context.Context, g *errgroup.Group, role config.Role, st *store.Store, cfg *config.Config, current *configfile.Current,
	logger *slog.Logger,
) error {
	if role != config.RoleAll && role != config.RoleWeb {
		return nil
	}
	webLogger := logger.With("listener", "web")
	authHandler, err := auth.New(auth.Config{Store: st, Current: current, WebURL: cfg.WebURLParsed(), Logger: webLogger})
	if err != nil {
		return err
	}
	// Insert-only River client: the dashboard enqueues re-runs, cancels and
	// reindexes, it never works jobs.
	queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{Logger: webLogger})
	if err != nil {
		return fmt.Errorf("river: %w", err)
	}
	api := webapi.New(webapi.Config{
		Store: st, Current: current, Auth: authHandler, UI: web.FS(),
		WebURL: cfg.WebURLParsed(), Version: version, Logger: webLogger, Actions: webapi.JobActions{Queue: queue}, Env: cfg.Env(),
	})
	g.Go(func() error { return api.Run(ctx) })
	g.Go(func() error { return server.ServeDrain(ctx, cfg.WebAddr, api.Handler(), webDrain, webLogger) })
	return nil
}

// startQueue starts the River client, retrying for a while when the
// database is briefly unavailable: right after a fresh cluster's initdb the
// first queue upsert can time out, and River cleans up after a failed
// start, so trying again is safe and beats a crash loop.
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
	if cfg.Executor == "local" {
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
// apply the current configuration and register the repositories its Apps
// reach, then do both again whenever it changes. While
// the configuration has an embedder it also keeps the index schema to it
// and enqueues an onboarding index job for every repository that has none.
func lead(
	ctx context.Context, st *store.Store, cfg *config.Config, current *configfile.Current, queue *river.Client[pgx.Tx],
	sweeper *executor.Kube, m *metrics.Metrics, configErrors *server.ConfigErrorGauge, logger *slog.Logger,
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
		Store: st, Current: current, Forges: &worker.ForgeCache{Build: worker.BuildForge}, Reach: worker.ReachRepositories,
		Dispatcher: ingest.NewService(st, queue),
		Logger:     logger, Metrics: m,
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
	return applyLoop(ctx, current, func(ctx context.Context, f *configfile.File) error {
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
	}, onboarder.Offer, refusedRetryInterval, configErrors, logger)
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
// transcripts, disabled repositories' indexes and dashboard sessions past
// their retention window.
const retentionSweepInterval = time.Hour

// retentionStore is the subset of *store.Store that retentionSweep needs,
// narrowed so it can be exercised in tests with a fake.
type retentionStore interface {
	SweepModelCalls(ctx context.Context, olderThan time.Duration) (int64, error)
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

// refusedRetryInterval is how often the leader re-applies a configuration
// the store refused. A refusal is expected to need a new configuration, but
// one misclassified race must not leave the store stale until the next edit.
const refusedRetryInterval = time.Minute

// applyLoop applies current's snapshot, then each replacement, until ctx
// ends, calling onApplied after each success. A snapshot the store refuses
// for its content (store.IsConfigContentError) must not end leadership, or
// every replica would crash-loop on it in turn: it is logged once per
// distinct error and raised on the gauge, the last applied state stays, and
// the loop waits for the next snapshot, retrying the refused one every
// retry. Any other error is returned, which ends the process for a restart.
func applyLoop(
	ctx context.Context, current *configfile.Current, apply func(context.Context, *configfile.File) error,
	onApplied func(context.Context) error, retry time.Duration, gauge *server.ConfigErrorGauge, logger *slog.Logger,
) error {
	applied, refused, logged := "", "", ""
	for {
		// Taken before Get, so a replacement that lands while applying still
		// wakes the loop.
		changed := current.Changed()
		f := current.Get()
		if h := f.Hash(); h != applied && h != refused {
			err := apply(ctx, f)
			switch {
			case err != nil && store.IsConfigContentError(err):
				refused = h
				gauge.Set(server.ConfigErrorApply, true)
				if err.Error() != logged {
					logged = err.Error()
					logger.Error("configuration refused by the store, keeping the last applied one", "hash", h[:12], "error", err)
				}
			case err != nil:
				return err
			default:
				applied, refused, logged = h, "", ""
				gauge.Set(server.ConfigErrorApply, false)
				logger.Info("configuration applied to the store", "hash", h[:12])
				if err := onApplied(ctx); err != nil {
					return err
				}
			}
		}
		var retryC <-chan time.Time
		if refused != "" {
			retryC = time.After(retry)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-retryC:
			refused = ""
		}
	}
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
