//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAppManifestFlow: a flow is claimed once, by the session that started
// it and before it expires, and its result is read once.
func TestAppManifestFlow(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	user, err := s.UpsertIdentity(ctx, SignInIdentity{Provider: "local", Origin: "local",
		Subject: "manifest-test-" + now.Format(time.RFC3339Nano), Login: "admin"}, now)
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	session := func() string {
		t.Helper()
		token, err := s.CreateSession(ctx, user.ID, "local", "local", SessionGrant{Role: RoleAdmin, Key: "k"}, now, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return token
	}
	mine, theirs := session(), session()

	state, err := s.CreateAppManifest(ctx, mine, "github", now)
	if err != nil {
		t.Fatalf("CreateAppManifest: %v", err)
	}
	if _, err := s.ClaimAppManifest(ctx, state, theirs, now); !errors.Is(err, ErrAppManifest) {
		t.Fatalf("another session's claim = %v, want ErrAppManifest", err)
	}
	if _, err := s.ClaimAppManifest(ctx, state, mine, now.Add(AppManifestTTL)); !errors.Is(err, ErrAppManifest) {
		t.Fatalf("an expired claim = %v, want ErrAppManifest", err)
	}
	if got, err := s.CollectAppManifests(ctx, mine); err != nil || len(got) != 0 {
		t.Fatalf("an unfinished flow is not collected: %+v, %v", got, err)
	}
	connection, err := s.ClaimAppManifest(ctx, state, mine, now)
	if err != nil || connection != "github" {
		t.Fatalf("ClaimAppManifest = %q, %v", connection, err)
	}
	if _, err := s.ClaimAppManifest(ctx, state, mine, now); !errors.Is(err, ErrAppManifest) {
		t.Fatalf("a second claim = %v, want ErrAppManifest", err)
	}
	want := AppManifestResult{Connection: "github", Slug: "kritik-org-1", ClientID: "Iv1.x", ClientSecret: "sealed"}
	if err := s.FinishAppManifest(ctx, state, want, now); err != nil {
		t.Fatalf("FinishAppManifest: %v", err)
	}
	if got, err := s.CollectAppManifests(ctx, theirs); err != nil || len(got) != 0 {
		t.Fatalf("another session collected %+v, %v", got, err)
	}
	got, err := s.CollectAppManifests(ctx, mine)
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("CollectAppManifests = %+v, %v", got, err)
	}
	if got, err := s.CollectAppManifests(ctx, mine); err != nil || len(got) != 0 {
		t.Fatalf("a result is read once: %+v, %v", got, err)
	}
}
