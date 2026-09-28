//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
)

// A connection the spec declares by the name of a file connection takes
// its row over only once the leader has disabled the file's, and never
// while the file's is live.
func TestApplyConfigTakesOverAConnection(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	base := parse(t, twoAccounts)
	if err := s.ApplyConfig(ctx, base, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	withSpec, err := configfile.Merge(base, configfile.InstanceSpec{Revision: 1, Spec: json.RawMessage(`{"connections":[{"name":"alpha-bot",` +
		`"forge":"github","accounts":["alpha"],"app":{"clientId":"Iv1.dash","privateKey":{"sealed":"` + configfiletest.Seal("k") +
		`"},"webhookSecret":{"sealed":"` + configfiletest.Seal("w") + `"}}}]}`)}, configfiletest.Opener)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	dash, _ := withSpec.Connection("alpha-bot")

	t.Run("not while the file's is live", func(t *testing.T) {
		tx, err := s.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = upsertConnection(ctx, tx, dash)
		if !errors.Is(err, ErrManagedBy) || !IsConfigContentError(err) {
			t.Fatalf("upsertConnection over a live file connection = %v, want a content ErrManagedBy", err)
		}
	})

	if err := s.ApplyConfig(ctx, withSpec, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	var origin string
	var enabled bool
	if err := s.owner.QueryRow(ctx, `SELECT managed_by, enabled FROM connections WHERE name = 'alpha-bot'`).Scan(&origin, &enabled); err != nil {
		t.Fatal(err)
	}
	if origin != "dashboard" || !enabled {
		t.Fatalf("alpha-bot managed_by %s, enabled %v; want the dashboard's, enabled", origin, enabled)
	}
	if err := s.ApplyConfig(ctx, base, "test"); err != nil {
		t.Fatalf("ApplyConfig back to the file: %v", err)
	}
	if err := s.owner.QueryRow(ctx, `SELECT managed_by FROM connections WHERE name = 'alpha-bot'`).Scan(&origin); err != nil || origin != "file" {
		t.Fatalf("alpha-bot managed_by %s, %v; want the file's again", origin, err)
	}
}
