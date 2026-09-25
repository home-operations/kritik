package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/home-operations/kritik/internal/configfile"
)

// githubScopes read the profile, its verified emails and org memberships.
var githubScopes = []string{"read:user", "user:email", "read:org"}

// newGitHubProvider signs in through a GitHub OAuth app on github.com or a
// GitHub Enterprise Server host.
func newGitHubProvider(s configfile.SignIn, redirect string, client *http.Client) *forgeProvider {
	web, api := "https://github.com", "https://api.github.com"
	if forgeHost(string(s.Type), s.Host) != githubHost {
		web = webBase(s.Host)
		api = web + "/api/v3"
	}
	p := newForgeProvider(s, web, api, redirect, githubScopes, client, githubAPI{})
	p.headers = map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
	return p
}

type githubAPI struct{}

func (githubAPI) identity(ctx context.Context, c apiClient) (Identity, error) {
	var u struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := c.getOK(ctx, "/user", &u); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return Identity{}, fmt.Errorf("%w: /user has no id or login", ErrForgeAPI)
	}
	id := Identity{Subject: strconv.FormatInt(u.ID, 10), Login: u.Login, Email: u.Email, DisplayName: u.Name, AvatarURL: u.AvatarURL}
	if err := verifiedEmail(ctx, c, &id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// orgRole reads the user's own membership of org, which needs read:org. A
// pending invitation, or any role but admin and member (a billing manager),
// is not membership.
func (githubAPI) orgRole(ctx context.Context, c apiClient, _, org string) (Role, error) {
	var m struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	status, err := c.get(ctx, "/user/memberships/orgs/"+url.PathEscape(org), &m)
	switch {
	case err != nil:
		return "", err
	case notMember(status):
		return "", nil
	case status != http.StatusOK:
		return "", fmt.Errorf("%w: organization membership: status %d", ErrForgeAPI, status)
	case m.State != "active":
		return "", nil
	}
	if r := Role(m.Role); r.Valid() {
		return r, nil
	}
	return "", nil
}
