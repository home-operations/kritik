package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/forge/forgejo"
	"github.com/home-operations/kritik/internal/forge/github"
)

// BuildForge constructs the forge client for an installation from its
// credentials in the configuration file. GitHub and Forgejo are
// implemented; GitLab follows the rollout order in the ADR.
func BuildForge(ctx context.Context, in *configfile.Installation, externalID int64, repo string) (forge.Client, error) {
	switch in.Forge {
	case configfile.ForgeGitHub:
		app, err := github.NewApp(in.App.ClientIDValue(), in.App.PrivateKeyValue().Value(), github.APIBase(in.Host))
		if err != nil {
			return nil, err
		}
		if externalID == 0 {
			// Declared before its installation webhook arrived: GitHub's
			// lookup is per repository, and the one being reviewed is one
			// the App can see by definition.
			owner, name, _ := strings.Cut(repo, "/")
			id, err := app.DiscoverInstallation(ctx, owner, name)
			if err != nil {
				return nil, err
			}
			externalID = id
		}
		return github.NewClient(app, externalID, in.Host)
	case configfile.ForgeForgejo:
		c, err := forgejo.NewClient(in.Host, in.TokenValue().Value(), nil)
		if err != nil {
			return nil, err
		}
		c.FetchToken = in.GitTokenValue().Value()
		return c, nil
	default:
		return nil, fmt.Errorf("worker: forge %s is not implemented yet", in.Forge)
	}
}
