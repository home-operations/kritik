package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubOrgRole(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		header  map[string]string
		body    string
		want    Role
		wantErr bool
	}{
		{name: "active member", status: 200, body: `{"state":"active","role":"member"}`, want: RoleMember},
		{name: "active admin", status: 200, body: `{"state":"active","role":"admin"}`, want: RoleAdmin},
		{name: "pending", status: 200, body: `{"state":"pending","role":"admin"}`},
		{name: "billing manager", status: 200, body: `{"state":"active","role":"billing_manager"}`},
		{name: "not found", status: 404},
		{name: "forbidden: OAuth app access restricted", status: 403},
		{name: "forbidden: primary rate limit", status: 403, header: map[string]string{"X-RateLimit-Remaining": "0"}, wantErr: true},
		{name: "forbidden: secondary rate limit", status: 403, header: map[string]string{"Retry-After": "60"}, wantErr: true},
		{name: "server error", status: 502, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/user/memberships/orgs/acme" || r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("request %s with %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			got, err := githubAPI{}.orgRole(context.Background(), apiClient{base: srv.URL, token: "tok", client: srv.Client()}, "alice", "acme")
			if tt.wantErr != (err != nil) || (err != nil && !errors.Is(err, ErrForgeAPI)) || got != tt.want {
				t.Fatalf("orgRole = %q, %v; want %q, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
