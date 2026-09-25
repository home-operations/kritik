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
}

func TestLoginStateConsumedOnce(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	state, err := s.CreateLoginState(ctx, LoginState{Provider: "gh", Nonce: "n", PKCEVerifier: "v", ReturnTo: "#/x"}, now)
	if err != nil {
		t.Fatalf("CreateLoginState: %v", err)
	}
	ls, err := s.ConsumeLoginState(ctx, state, now)
	if err != nil || ls != (LoginState{Provider: "gh", Nonce: "n", PKCEVerifier: "v", ReturnTo: "#/x"}) {
		t.Fatalf("ConsumeLoginState = %+v, %v", ls, err)
	}
	if _, err := s.ConsumeLoginState(ctx, state, now); !errors.Is(err, ErrLoginState) {
		t.Fatalf("second ConsumeLoginState = %v, want ErrLoginState", err)
	}
}
