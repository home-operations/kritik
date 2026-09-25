//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReplaceForgeMemberships(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoTenants), "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha, beta := tenantID(t, s, "alpha"), tenantID(t, s, "beta")
	now := time.Now()
	acct, err := s.UpsertIdentity(ctx, SignInIdentity{Provider: "gh", Subject: "store-test-" + now.Format(time.RFC3339Nano), Login: "x"}, now)
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	notYetApplied := "00000000-0000-5000-8000-000000000001"
	tests := []struct {
		name   string
		grants []Grant
		want   map[string]Role
	}{
		{"a tenant the leader has not created is skipped",
			[]Grant{{TenantID: notYetApplied, Role: RoleAdmin}, {TenantID: alpha, Role: RoleMember}, {TenantID: beta, Role: RoleAdmin}},
			map[string]Role{alpha: RoleMember, beta: RoleAdmin}},
		{"a lapsed membership is dropped", []Grant{{TenantID: beta, Role: RoleMember}}, map[string]Role{beta: RoleMember}},
		{"no grants", nil, map[string]Role{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.ReplaceForgeMemberships(ctx, acct.ID, tt.grants, now); err != nil {
				t.Fatalf("ReplaceForgeMemberships: %v", err)
			}
			got, err := s.Memberships(ctx, acct.ID)
			if err != nil {
				t.Fatalf("Memberships: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("memberships = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("memberships = %v, want %v", got, tt.want)
				}
			}
		})
	}
	if err := s.ReplaceForgeMemberships(ctx, acct.ID, []Grant{{TenantID: alpha, Role: "owner"}}, now); err == nil {
		t.Fatal("an invalid role was stored")
	}

	// Sources are kept apart and combine to the higher role.
	email := "store-invite-" + now.Format("150405.000000") + "@example.com"
	if _, err := s.App().Exec(ctx, `INSERT INTO invites (id, tenant_id, email, role, expires_at) VALUES (gen_random_uuid(), $1, $2, 'member', $3)`,
		alpha, email, now.Add(time.Hour)); err != nil {
		t.Fatalf("insert invite: %v", err)
	}
	if n, err := s.AcceptInvites(ctx, acct.ID, email, now); err != nil || n != 1 {
		t.Fatalf("AcceptInvites = %d, %v", n, err)
	}
	if err := s.ReplaceForgeMemberships(ctx, acct.ID, []Grant{{TenantID: alpha, Role: RoleAdmin}}, now); err != nil {
		t.Fatalf("ReplaceForgeMemberships: %v", err)
	}
	if got, _ := s.Memberships(ctx, acct.ID); got[alpha] != RoleAdmin || len(got) != 1 {
		t.Fatalf("forge admin + member invite = %v, want admin", got)
	}
	if err := s.ReplaceForgeMemberships(ctx, acct.ID, nil, now); err != nil {
		t.Fatalf("ReplaceForgeMemberships: %v", err)
	}
	if got, _ := s.Memberships(ctx, acct.ID); got[alpha] != RoleMember || len(got) != 1 {
		t.Fatalf("lapsed forge admin + member invite = %v, want member", got)
	}
}

func TestLoginStateConsumedOnce(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	state, err := s.CreateLoginState(ctx, LoginState{Provider: "gh", Nonce: "n", PKCEVerifier: "v", ReturnTo: "#/x"}, "browser", now)
	if err != nil {
		t.Fatalf("CreateLoginState: %v", err)
	}
	if _, err := s.ConsumeLoginState(ctx, state, "another-browser", now); !errors.Is(err, ErrLoginState) {
		t.Fatalf("ConsumeLoginState from another browser = %v, want ErrLoginState", err)
	}
	ls, err := s.ConsumeLoginState(ctx, state, "browser", now)
	if err != nil || ls != (LoginState{Provider: "gh", Nonce: "n", PKCEVerifier: "v", ReturnTo: "#/x"}) {
		t.Fatalf("ConsumeLoginState = %+v, %v", ls, err)
	}
	if _, err := s.ConsumeLoginState(ctx, state, "browser", now); !errors.Is(err, ErrLoginState) {
		t.Fatalf("second ConsumeLoginState = %v, want ErrLoginState", err)
	}
}
