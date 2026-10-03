package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/forge/github"
	"github.com/home-operations/kritika/internal/metrics"
	"github.com/home-operations/kritika/internal/store"
)

// Apps holds one GitHub App per connection, built on first use and kept
// for the life of the process, as the configuration is, so the clients
// and the listings of a connection share its installation tokens: minted
// once an hour each, however often the connection is polled.
type Apps struct {
	// Metrics may be nil. Rate limits the forge answers with are logged
	// and counted on it.
	Metrics *metrics.Metrics

	mu   sync.Mutex
	apps map[string]*github.App
}

// app is connection in's App.
func (a *Apps) app(in *configfile.Connection) (*github.App, error) {
	if in.Forge != configfile.ForgeGitHub {
		return nil, fmt.Errorf("worker: forge %s is not implemented yet", in.Forge)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if app, ok := a.apps[in.Name]; ok {
		return app, nil
	}
	app, err := github.NewApp(in.App.ClientIDValue(), in.App.PrivateKeyValue().Value(), "")
	if err != nil {
		return nil, err
	}
	name := in.Name
	app.OnRateLimit = func(wait time.Duration, waited bool) {
		outcome := "waited"
		if !waited {
			outcome = "refused"
		}
		slog.Warn("github rate limit hit", "connection", name, "wait", wait.Round(time.Second), "outcome", outcome)
		a.Metrics.ForgeRateLimited(name, outcome)
	}
	if a.apps == nil {
		a.apps = map[string]*github.App{}
	}
	a.apps[name] = app
	return app, nil
}

// Build constructs the forge client for a connection, for repositories of
// repo's owner: ForgeCache's Build.
func (a *Apps) Build(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error) {
	app, err := a.app(in)
	if err != nil {
		return nil, err
	}
	// An App is installed, and mints tokens, once per account: the
	// installation that sees repo is its owner's.
	owner, name, _ := strings.Cut(repo, "/")
	id, err := app.DiscoverInstallation(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	return github.NewClient(app, id)
}

// Reach lists the repositories connection in's App reaches, by the
// lowercased login of the account each is under, as the store registers
// them: the poller's Reach.
func (a *Apps) Reach(ctx context.Context, in *configfile.Connection) (map[string][]store.ReachedRepository, error) {
	app, err := a.app(in)
	if err != nil {
		return nil, err
	}
	reach, err := app.Reach(ctx, in.Accounts)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]store.ReachedRepository, len(reach))
	for _, r := range reach {
		repos := make([]store.ReachedRepository, 0, len(r.Repositories))
		for _, repo := range r.Repositories {
			repos = append(repos, store.ReachedRepository{
				FullName: repo.FullName, DefaultBranch: repo.DefaultBranch, Traits: &configfile.RepoTraits{Archived: repo.Archived, Fork: repo.Fork},
			})
		}
		out[strings.ToLower(r.Account)] = repos
	}
	return out, nil
}

// ForgeCache is the forge.Clients over configured GitHub Apps. One client
// per App and repository owner, built on first use and kept for the life of
// the process, as the configuration is.
type ForgeCache struct {
	Build func(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error)

	mu      sync.Mutex
	clients map[string]forge.Client
	// building shares one build per key among the callers that find none
	// cached, outside mu: a build asks the forge, and a slow answer must
	// not hold up the callers of every other connection and owner.
	building singleflight.Group
}

// forgeBuildTimeout bounds a client's build, which runs on no caller's ctx.
const forgeBuildTimeout = time.Minute

// For implements forge.Clients. A caller whose ctx ends while a build is
// under way returns with ctx's error; the build goes on for the others,
// whichever caller started it.
func (c *ForgeCache) For(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error) {
	owner, _, _ := strings.Cut(repo, "/")
	key := in.Name + "/" + strings.ToLower(owner)
	c.mu.Lock()
	client, ok := c.clients[key]
	c.mu.Unlock()
	if ok {
		return client, nil
	}
	results := c.building.DoChan(key, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgeBuildTimeout)
		defer cancel()
		client, err := c.Build(bctx, in, repo)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.clients == nil {
			c.clients = map[string]forge.Client{}
		}
		c.clients[key] = client
		c.mu.Unlock()
		return client, nil
	})
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("worker: forge client for %s: %w", key, ctx.Err())
	case res := <-results:
		if res.Err != nil {
			return nil, res.Err
		}
		client, _ := res.Val.(forge.Client)
		return client, nil
	}
}
