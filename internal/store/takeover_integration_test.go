//go:build integration

package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// A connection name another account held is never handed to a new
// account, even once its first account is gone: its repositories and
// history stay with the account that made them.
func TestApplyConfigKeepsConnectionWithItsAccount(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	base := parse(t, twoAccounts)
	merge := func(slug string) *configfile.File {
		t.Helper()
		f, err := configfile.Merge(base, []configfile.DashboardAccount{
			{Slug: slug, Spec: dashboardSpec(slug, "held-bot"), Revision: 1},
		}, plainOpener{})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		return f
	}
	first, second := merge("held-a"), merge("held-b")
	if err := s.ApplyConfig(ctx, first, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if err := s.ApplyConfig(ctx, base, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	err := s.ApplyConfig(ctx, second, "test")
	if !errors.Is(err, ErrManagedBy) || !IsConfigContentError(err) {
		t.Fatalf("ApplyConfig of another account's connection = %v, want a content ErrManagedBy", err)
	}
	var owner string
	if err := s.owner.QueryRow(ctx, `SELECT t.slug FROM connections i JOIN accounts t ON t.id = i.account_id
		WHERE i.name = 'held-bot'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "held-a" {
		t.Fatalf("held-bot belongs to %s, want held-a", owner)
	}

	t.Run("the web role sees it as held elsewhere", func(t *testing.T) {
		tests := []struct {
			name    string
			account string
			names   []string
			want    []string
		}{
			{"another account, no accounts row yet", "held-new", []string{"held-bot", "free-bot"}, []string{"held-bot"}},
			{"another account with a row", "alpha", []string{"held-bot"}, []string{"held-bot"}},
			{"its own account", "held-a", []string{"held-bot"}, nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				id := (&configfile.Account{Slug: tt.account}).ID()
				var got []string
				err := s.WithAccount(ctx, id, func(tx pgx.Tx) error {
					var err error
					got, err = ConnectionsHeldElsewhere(ctx, tx, id, tt.names)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("held = %v, want %v", got, tt.want)
				}
			})
		}
		var n int
		if err := s.owner.QueryRow(ctx, `SELECT count(*) FROM connections WHERE name = 'free-bot'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("probe left %d free-bot rows (%v)", n, err)
		}
	})

	t.Run("an account row outlives its account", func(t *testing.T) {
		for slug, want := range map[string]bool{"held-a": true, "held-never": false} {
			id := (&configfile.Account{Slug: slug}).ID()
			var got bool
			err := s.WithAccount(ctx, id, func(tx pgx.Tx) error {
				var err error
				got, err = AccountRowExists(ctx, tx, id)
				return err
			})
			if err != nil || got != want {
				t.Errorf("AccountRowExists(%s) = %v, %v; want %v", slug, got, err, want)
			}
		}
	})
}
