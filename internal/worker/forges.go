package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/forge/github"
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

// credentialFingerprint identifies what BuildForge builds a client from, so a
// cached client is rebuilt once any of it changes: an App key rotated
// through the file or the dashboard. It is a hash, never the
// material itself.
func credentialFingerprint(in *configfile.Connection) string {
	parts := []string{string(in.Forge), in.App.ClientIDValue(), in.App.PrivateKeyValue().Value()}
	h := sha256.New()
	for _, p := range parts {
		// Length-prefixed, so no two different part lists hash alike.
		h.Write([]byte(strconv.Itoa(len(p)) + ":" + p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
