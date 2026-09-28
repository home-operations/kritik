package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
)

// githubScopes read the profile, its verified emails and org memberships.
var githubScopes = []string{"read:user", "user:email", "read:org"}

// newGitHubProvider signs in through a GitHub OAuth app, or a GitHub App's
// own client, on github.com.
func newGitHubProvider(s *configfile.SignIn, redirect string, client *http.Client) *forgeProvider {
	p := newForgeProvider(s, "https://github.com", "https://api.github.com", redirect, githubScopes, client, githubAPI{})
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

// member reads the user's own membership of org, which needs read:org, or
// a GitHub App's members permission. A pending invitation, or any role but
// admin and member (a billing manager), is not membership.
func (githubAPI) member(ctx context.Context, c apiClient, _, org string) (bool, error) {
	var m struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	status, header, err := c.get(ctx, "/user/memberships/orgs/"+url.PathEscape(org), &m)
	switch {
	case err != nil:
		return false, err
	case status == http.StatusForbidden && githubRateLimited(header):
		return false, fmt.Errorf("%w: organization membership: rate limited", ErrForgeAPI)
	case notMember(status):
		return false, nil
	case status != http.StatusOK:
		return false, fmt.Errorf("%w: organization membership: status %d", ErrForgeAPI, status)
	}
	return m.State == "active" && (m.Role == "admin" || m.Role == "member"), nil
}

// githubMaxPages bounds how many pages of organizations or teams a sign-in
// reads: a hundred each, far beyond any real account.
const githubMaxPages = 10

// mappingVars reads the organizations the user is an active member of and
// the teams they are on, for the role mapping.
func (githubAPI) mappingVars(ctx context.Context, c apiClient, id Identity) (map[string]any, error) {
	orgs := []string{}
	err := githubPages(ctx, c, "/user/memberships/orgs?state=active", func(raw json.RawMessage) error {
		var page []struct {
			State        string `json:"state"`
			Role         string `json:"role"`
			Organization struct {
				Login string `json:"login"`
			} `json:"organization"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, m := range page {
			if m.State == "active" && (m.Role == "admin" || m.Role == "member") {
				orgs = append(orgs, m.Organization.Login)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	teams := []string{}
	err = githubPages(ctx, c, "/user/teams?", func(raw json.RawMessage) error {
		var page []struct {
			Slug         string `json:"slug"`
			Organization struct {
				Login string `json:"login"`
			} `json:"organization"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, t := range page {
			teams = append(teams, t.Organization.Login+"/"+t.Slug)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"login": id.Login, "email": id.Email, "orgs": orgs, "teams": teams}, nil
}

// githubPages calls fn with each page of the list at path, which must end
// in "?" or carry a query, until a page comes back short.
func githubPages(ctx context.Context, c apiClient, path string, fn func(json.RawMessage) error) error {
	sep := "&"
	if strings.HasSuffix(path, "?") {
		sep = ""
	}
	for page := 1; page <= githubMaxPages; page++ {
		var raw json.RawMessage
		if err := c.getOK(ctx, fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), &raw); err != nil {
			return err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrForgeAPI, path, err)
		}
		if err := fn(raw); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrForgeAPI, path, err)
		}
		if len(items) < 100 {
			return nil
		}
	}
	return nil
}

// githubRateLimited reports whether a 403 is GitHub's primary or secondary
// rate limit rather than a refusal to disclose membership.
func githubRateLimited(h http.Header) bool {
	return h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != ""
}
