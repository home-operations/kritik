// Command kritik reviews GitHub, GitLab and Forgejo pull requests against an
// index of the repository. One binary serves every role; --role selects
// which part of the service this process runs.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
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

	"github.com/home-operations/kritik/internal/config"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configsource"
	"github.com/home-operations/kritik/internal/egress"
	"github.com/home-operations/kritik/internal/executor"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/metrics"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/poller"
	"github.com/home-operations/kritik/internal/runner"
	"github.com/home-operations/kritik/internal/server"
	"github.com/home-operations/kritik/internal/store"
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
	roleFlag := pflag.String("role", string(config.RoleAll), "process role: all, ingest, worker or runner")
	pflag.Parse()
	role, err := config.ParseRole(*roleFlag)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var runSpec runner.Spec
	switch role {
	case config.RoleAll, config.RoleWorker:
		err = cfg.ValidateWorker()
	case config.RoleRunner:
		if err = cfg.ValidateRunner(); err == nil {
			runSpec, err = runner.ReadSpec(cfg.RunSpecFile)
		}
	}
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
		"embedding", cfg.EmbeddingEnabled(),
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
	ownerURL := ""
	if role == config.RoleAll || role == config.RoleWorker {
		ownerURL = cfg.DatabaseOwnerURL
	}
	st, err := openStore(ctx, store.Options{
		AppURL: cfg.DatabaseURL, OwnerURL: ownerURL,
		AppRole: cfg.DatabaseAppRole, RunnerRole: cfg.DatabaseRunnerRole, Logger: logger,
	}, logger)
	if err != nil {
		return err
	}
	defer st.Close()

	// The runner gets everything it needs from its Job spec; every other role
	// is driven by the configuration file, merged with the dashboard's
	// tenants, and must not start without it.
	var current *configfile.Current
	var exec executor.Executor
	if role != config.RoleRunner {
		// current is the last good merged file; the leader applies it on
		// election and on every reload, followers only compare hashes.
		src := &configsource.Source{Store: st, Keyring: cfg.DashboardKeyring(), Logger: logger, Errors: configErrors}
		file, err := src.Load(ctx, cfg.ConfigFile)
		if err != nil {
			return err
		}
		logConfig(logger, file, "configuration loaded")
		current = src.Current
		g.Go(func() error { return src.Run(ctx, cfg.ConfigFile, cfg.ConfigReloadInterval) })
		g.Go(func() error {
			return reportDrift(ctx, st, current, drift, cfg.ConfigReloadInterval)
		})
		if st.LeaderEligible() {
			// Only an all or worker role holds the owner DSN, so a leader
			// always has the executor it would sweep up after.
			exec, err = newExecutor(ctx, cfg, logger)
			if err != nil {
				return err
			}
			sweeper, _ := exec.(*executor.Kube)
			hostname, _ := os.Hostname()
			// Insert-only client: the leader enqueues onboarding index jobs.
			leaderQueue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{Logger: logger})
			if err != nil {
				return fmt.Errorf("river: %w", err)
			}
			g.Go(func() error {
				return st.RunAsLeader(ctx, cfg.LeaderRetryInterval, func(ctx context.Context) error {
					return lead(ctx, st, cfg, current, leaderQueue, sweeper, m, configErrors, hostname, logger)
				})
			})
		} else if role != config.RoleIngest {
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
		handler := ingest.NewHandler(current, ingest.NewService(st, queue), logger)
		handler.Metrics = m
		hooks := server.NewHooks(cfg.Addr, handler, logger)
		g.Go(func() error { return hooks.Run(ctx) })
	}
	if role == config.RoleAll || role == config.RoleWorker {
		if exec == nil {
			if exec, err = newExecutor(ctx, cfg, logger); err != nil {
				return err
			}
		}
		embedder := newEmbedder(cfg)
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
			Base: base, Executor: exec, Completers: completers, Embedder: embedder, EmbedModel: cfg.EmbedModel, Deadline: cfg.RunnerDeadline,
			GatewayURL: cfg.GatewayURL, GatewayTokenTTL: cfg.GatewayTokenTTL,
		})
		river.AddWorker(workers, &worker.FollowUp{Base: base, Completers: completers})
		river.AddWorker(workers, &worker.Index{
			Base: base, Executor: exec, Embedder: embedder, EmbedModel: cfg.EmbedModel, EmbedDims: cfg.EmbedDims, Deadline: cfg.RunnerDeadline,
		})
		queue, err := river.NewClient(riverpgxv5.New(st.App()), &river.Config{
			Logger: logger,
			// Review and index workers set their own timeouts from the
			// runner deadline; rescue must wait out the longest of them.
			RescueStuckJobsAfter: worker.RescueStuckJobsAfter,
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
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

// enqueueMissingIndexes gives every enabled repository without an active
// index generation an onboarding job. Index jobs are unique while queued
// or running, so repeating this on every configuration change is cheap.
func enqueueMissingIndexes(ctx context.Context, st *store.Store, queue *river.Client[pgx.Tx], logger *slog.Logger) error {
	refs, err := st.RepositoriesWithoutIndex(ctx)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	params := make([]river.InsertManyParams, 0, len(refs))
	for _, r := range refs {
		params = append(params, river.InsertManyParams{Args: jobs.IndexArgs{TenantID: r.TenantID, RepositoryID: r.ID, Trigger: "onboard"}})
	}
	results, err := queue.InsertMany(ctx, params)
	if err != nil {
		return fmt.Errorf("river: enqueue index jobs: %w", err)
	}
	enqueued := 0
	for _, r := range results {
		if !r.UniqueSkippedAsDuplicate {
			enqueued++
		}
	}
	logger.Info("onboarding index jobs", "repositories", len(refs), "enqueued", enqueued)
	return nil
}

// newEmbedder builds the deployment embedder, or nil when indexing is off.
func newEmbedder(cfg *config.Config) model.Embedder {
	if !cfg.EmbeddingEnabled() {
		return nil
	}
	e := model.NewOpenAIEmbedder(cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel, cfg.EmbedDims)
	e.MaxBatch, e.MaxBatchChars, e.MaxItemChars = cfg.EmbedMaxBatch, cfg.EmbedMaxBatchChars, cfg.EmbedMaxItemChars
	return e
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
// apply the current file, then re-apply whenever the file changes. With an
// embedder configured it also owns the index schema and enqueues an
// onboarding index job for every repository that has none.
func lead(
	ctx context.Context, st *store.Store, cfg *config.Config, current *configfile.Current, queue *river.Client[pgx.Tx],
	sweeper *executor.Kube, m *metrics.Metrics, configErrors *server.ConfigErrorGauge, leader string, logger *slog.Logger,
) error {
	if err := st.Migrate(ctx, cfg.DatabaseAppRole, cfg.DatabaseRunnerRole); err != nil {
		return err
	}
	if cfg.EmbeddingEnabled() {
		if err := st.EnsureIndexSchema(ctx, cfg.DatabaseAppRole, cfg.EmbedModel, cfg.EmbedDims, cfg.ReindexOnModelChange); err != nil {
			return err
		}
	}
	// The backstop poll is a leader duty: one lister per installation.
	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	go func() {
		_ = (&poller.Poller{
			Store: st, Current: current, Forges: &worker.ForgeCache{Build: worker.BuildForge},
			Dispatcher: ingest.NewService(st, queue), Interval: cfg.PollInterval, Lookback: cfg.PollLookback,
			Logger: logger, Metrics: m,
		}).Run(pollCtx)
	}()
	// So is deleting, by name, run Secrets a dead worker left without an
	// owner. Like the poller it walks the configured tenants, each under
	// its own row-level security scope.
	if sweeper != nil {
		go sweeper.RunSecretSweeper(pollCtx, st, func() []string {
			tenants := current.Get().Tenants
			ids := make([]string, 0, len(tenants))
			for i := range tenants {
				ids = append(ids, tenants[i].ID())
			}
			return ids
		}, secretSweepInterval)
	}
	return applyLoop(ctx, current, func(ctx context.Context, f *configfile.File) error {
		return st.ApplyConfig(ctx, f, leader)
	}, func(ctx context.Context) error {
		if !cfg.EmbeddingEnabled() {
			return nil
		}
		return enqueueMissingIndexes(ctx, st, queue, logger)
	}, configErrors, logger)
}

// applyLoop applies current's snapshot, then each replacement, until ctx
// ends, calling onApplied after each success. A snapshot the store refuses
// for its content (store.IsConfigContentError) must not end leadership, or
// every replica would crash-loop on it in turn: it is logged once per
// distinct error and raised on the gauge, the last applied state stays, and
// the loop waits for the next snapshot. Any other error is returned.
func applyLoop(
	ctx context.Context, current *configfile.Current, apply func(context.Context, *configfile.File) error,
	onApplied func(context.Context) error, gauge *server.ConfigErrorGauge, logger *slog.Logger,
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
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
	}
}

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
	installations := 0
	for _, t := range f.Tenants {
		installations += len(t.Installations)
	}
	logger.Info(msg, "providers", len(f.Providers), "tenants", len(f.Tenants), "installations", installations)
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
