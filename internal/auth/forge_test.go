package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifiedEmail(t *testing.T) {
	profile := Identity{Login: "alice", Email: "profile@example.com"}
	tests := []struct {
		name    string
		status  int
		body    string
		want    Identity
		wantErr bool
	}{
		{name: "primary verified replaces the profile's", status: 200,
			body: `[{"email":"other@example.com","primary":false,"verified":true},{"email":"alice@example.com","primary":true,"verified":true}]`,
			want: Identity{Login: "alice", Email: "alice@example.com", EmailVerified: true}},
		{name: "primary unverified stands unverified", status: 200,
			body: `[{"email":"alice@example.com","primary":true,"verified":false}]`, want: profile},
		{name: "verified but not primary", status: 200,
			body: `[{"email":"other@example.com","primary":false,"verified":true}]`, want: profile},
		{name: "empty primary", status: 200, body: `[{"email":"","primary":true,"verified":true}]`, want: profile},
		{name: "no scope to list addresses", status: 403, want: profile},
		{name: "not found", status: 404, want: profile},
		{name: "server error", status: 500, want: profile},
		{name: "unreadable answer", status: 200, body: `{"not":"a list"}`, want: profile, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/user/emails" || r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("request %s with %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			id := profile
			err := verifiedEmail(context.Background(), apiClient{base: srv.URL, token: "tok", client: srv.Client()}, &id)
			if tt.wantErr != (err != nil) || (err != nil && !errors.Is(err, ErrForgeAPI)) || id != tt.want {
				t.Fatalf("verifiedEmail = %v, id = %+v; want %+v, error %v", err, id, tt.want, tt.wantErr)
			}
		})
	}
}
