//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSessionGrant: a session keeps the grant its sign-in decided, and a
// grant without a role is refused.
func TestSessionGrant(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	acct, err := s.UpsertIdentity(ctx, SignInIdentity{Provider: "github", Origin: "github:https://github.com",
		Subject: "store-test-" + now.Format(time.RFC3339Nano), Login: "x"}, now)
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	for _, g := range []SessionGrant{
		{Role: RoleAdmin, Key: "k1"},
		{Role: RoleMember, AllAccounts: true, Key: "k2"},
		{Role: RoleMember, Accounts: []string{"github/org-1", "github/user-1"}, Key: "k3"},
	} {
		token, err := s.CreateSession(ctx, acct.ID, "github", "github:https://github.com", g, now, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("CreateSession(%+v): %v", g, err)
		}
		sess, err := s.LookupSession(ctx, token, now)
		if err != nil {
			t.Fatalf("LookupSession: %v", err)
		}
		got := sess.Grant
		if got.Role != g.Role || got.AllAccounts != g.AllAccounts || got.Key != g.Key || len(got.Accounts) != len(g.Accounts) {
			t.Fatalf("grant = %+v, want %+v", got, g)
		}
		for i := range g.Accounts {
			if got.Accounts[i] != g.Accounts[i] {
				t.Fatalf("grant = %+v, want %+v", got, g)
			}
		}
	}
	if _, err := s.CreateSession(ctx, acct.ID, "github", "github:https://github.com", SessionGrant{}, now, now.Add(time.Hour)); err == nil {
		t.Fatal("a grant without a role was stored")
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

// Unauthenticated sign-in starts each leave a row; past the cap they are
// refused until rows expire, rather than growing the table without bound.
func TestCreateLoginStateCap(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `DELETE FROM login_states WHERE provider = 'cap-test'`)
	})
	if _, err := s.owner.Exec(ctx, `INSERT INTO login_states (state_hash, provider, nonce, pkce_verifier, expires_at, browser_hash)
		SELECT sha256(('cap-' || g)::bytea), 'cap-test', 'n', 'v', $1, sha256('b'::bytea)
		FROM generate_series(1, $2::int - (SELECT count(*)::int FROM login_states WHERE expires_at > $3)) g`,
		now.Add(time.Minute), MaxLoginStates, now); err != nil {
		t.Fatal(err)
	}
	ls := LoginState{Provider: "cap-test", Nonce: "n", PKCEVerifier: "v"}
	if _, err := s.CreateLoginState(ctx, ls, "browser", now); !errors.Is(err, ErrLoginStatesFull) {
		t.Fatalf("CreateLoginState at the cap = %v, want ErrLoginStatesFull", err)
	}
	// Once they expire the rows no longer count.
	if _, err := s.CreateLoginState(ctx, ls, "browser", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("CreateLoginState after expiry = %v", err)
	}
}
