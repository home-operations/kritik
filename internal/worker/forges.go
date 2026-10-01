package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/forge"
	"github.com/home-operations/kritika/internal/forge/github"
	"github.com/home-operations/kritika/internal/store"
)

// BuildForge constructs the forge client for a connection from its
// credentials in the configuration file, for repositories of repo's owner.
func BuildForge(ctx context.Context, in *configfile.Connection, repo string) (forge.Client, error) {
	switch in.Forge {
	case configfile.ForgeGitHub:
		app, err := github.NewApp(in.App.ClientIDValue(), in.App.PrivateKeyValue().Value(), "")
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
	default:
		return nil, fmt.Errorf("worker: forge %s is not implemented yet", in.Forge)
	}
}

// ReachRepositories lists the repositories connection in's App reaches,
// by the lowercased login of the account each is under, as the store
// registers them.
func ReachRepositories(ctx context.Context, in *configfile.Connection) (map[string][]store.ReachedRepository, error) {
	if in.Forge != configfile.ForgeGitHub {
		return nil, fmt.Errorf("worker: forge %s is not implemented yet", in.Forge)
	}
	app, err := github.NewApp(in.App.ClientIDValue(), in.App.PrivateKeyValue().Value(), "")
	if err != nil {
		return nil, err
	}
	reach, err := app.Reach(ctx, in.Accounts)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]store.ReachedRepository, len(reach))
	for _, a := range reach {
		repos := make([]store.ReachedRepository, 0, len(a.Repositories))
		for _, r := range a.Repositories {
			repos = append(repos, store.ReachedRepository{
				FullName: r.FullName, DefaultBranch: r.DefaultBranch, Traits: &configfile.RepoTraits{Archived: r.Archived, Fork: r.Fork},
			})
		}
		out[strings.ToLower(a.Account)] = repos
	}
	return out, nil
}
