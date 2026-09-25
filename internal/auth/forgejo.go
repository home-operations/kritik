package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/home-operations/kritik/internal/configfile"
)

// forgejoScopes read the profile, its emails and org memberships.
var forgejoScopes = []string{"read:user", "read:organization"}

// newForgejoProvider signs in through a Forgejo OAuth2 application.
func newForgejoProvider(s configfile.SignIn, redirect string, client *http.Client) *forgeProvider {
	web := webBase(s.Host)
	return newForgeProvider(s, web, web+"/api/v1", redirect, forgejoScopes, client, forgejoAPI{})
}

type forgejoAPI struct{}

func (forgejoAPI) identity(ctx context.Context, c apiClient) (Identity, error) {
	var u struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		FullName  string `json:"full_name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := c.getOK(ctx, "/user", &u); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return Identity{}, fmt.Errorf("%w: /user has no id or login", ErrForgeAPI)
	}
	id := Identity{Subject: strconv.FormatInt(u.ID, 10), Login: u.Login, Email: u.Email, DisplayName: u.FullName, AvatarURL: u.AvatarURL}
	if err := verifiedEmail(ctx, c, &id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// orgRole asks whether the user is a member of org (204, or 404 when not)
// and, if so, whether they own or administer it.
func (forgejoAPI) orgRole(ctx context.Context, c apiClient, login, org string) (Role, error) {
	status, _, err := c.get(ctx, "/orgs/"+url.PathEscape(org)+"/members/"+url.PathEscape(login), nil)
	switch {
	case err != nil:
		return "", err
	case notMember(status):
		return "", nil
	case status != http.StatusNoContent:
		return "", fmt.Errorf("%w: organization membership: status %d", ErrForgeAPI, status)
	}
	var perms struct {
		IsOwner bool `json:"is_owner"`
		IsAdmin bool `json:"is_admin"`
	}
	if err := c.getOK(ctx, "/users/"+url.PathEscape(login)+"/orgs/"+url.PathEscape(org)+"/permissions", &perms); err != nil {
		return "", err
	}
	if perms.IsOwner || perms.IsAdmin {
		return RoleAdmin, nil
	}
	return RoleMember, nil
}
