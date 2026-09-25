//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// plainOpener opens "sealed:<plain>" to <plain>.
type plainOpener struct{}

func (plainOpener) Open(sealed string) ([]byte, error) {
	plain, ok := strings.CutPrefix(sealed, "sealed:")
	if !ok {
		return nil, errors.New("not sealed")
	}
	return []byte(plain), nil
}

func dashboardSpec(slug, inst string) json.RawMessage {
	return json.RawMessage(`{"slug":"` + slug + `","installations":[{"name":"` + inst + `","forge":"forgejo","account":"` + slug + `",` +
		`"token":{"sealed":"sealed:tok"},"webhookSecret":{"sealed":"sealed:wh"}}],"repositories":[{"name":"` + slug + `/one"}]}`)
}

func resetDashboard(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.owner.Exec(context.Background(), `DELETE FROM dashboard_tenants`); err != nil {
		t.Fatalf("reset dashboard_tenants: %v", err)
	}
}

func inTx(t *testing.T, s *Store, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := s.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestDashboardTenants(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	resetDashboard(t, s)
	var account string
	if err := s.app.QueryRow(ctx, `INSERT INTO accounts (display_name) VALUES ('op') RETURNING id`).Scan(&account); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	fingerprint := func(t *testing.T) string {
		t.Helper()
		fp, err := s.DashboardFingerprint(ctx)
		if err != nil {
			t.Fatalf("DashboardFingerprint: %v", err)
		}
		return fp
	}
	empty := fingerprint(t)
	put := func(slug string, expected int64) (int64, error) {
		var rev int64
		err := inTx(t, s, func(tx pgx.Tx) error {
			var err error
			rev, err = s.PutDashboardTenant(ctx, tx, slug, dashboardSpec(slug, slug+"-bot"), expected, account)
			return err
		})
		return rev, err
	}
	del := func(slug string, expected int64) error {
		return inTx(t, s, func(tx pgx.Tx) error { return s.DeleteDashboardTenant(ctx, tx, slug, expected) })
	}

	steps := []struct {
		name    string
		do      func() (int64, error)
		wantRev int64
		wantErr error
	}{
		{"create", func() (int64, error) { return put("gamma", 0) }, 1, nil},
		{"create again conflicts", func() (int64, error) { return put("gamma", 0) }, 0, ErrDashboardConflict},
		{"update at current revision", func() (int64, error) { return put("gamma", 1) }, 2, nil},
		{"update at stale revision conflicts", func() (int64, error) { return put("gamma", 1) }, 0, ErrDashboardConflict},
		{"update a missing tenant conflicts", func() (int64, error) { return put("nope", 3) }, 0, ErrDashboardConflict},
		{"delete at stale revision conflicts", func() (int64, error) { return 0, del("gamma", 1) }, 0, ErrDashboardConflict},
		{"delete a missing tenant", func() (int64, error) { return 0, del("nope", 1) }, 0, ErrNotFound},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			rev, err := st.do()
			if !errors.Is(err, st.wantErr) || rev != st.wantRev {
				t.Fatalf("got rev %d, err %v; want rev %d, err %v", rev, err, st.wantRev, st.wantErr)
			}
		})
	}

	t.Run("read one with its writers", func(t *testing.T) {
		err := inTx(t, s, func(tx pgx.Tx) error {
			d, m, err := s.DashboardTenant(ctx, tx, "gamma")
			if err != nil {
				return err
			}
			if d.Slug != "gamma" || d.Revision != 2 || m.CreatedBy != account || m.UpdatedBy != account || m.UpdatedAt.IsZero() {
				t.Fatalf("got %+v %+v", d, m)
			}
			if _, err := configfile.DecodeTenant(d); err != nil {
				t.Fatalf("stored spec does not decode: %v", err)
			}
			_, _, err = s.DashboardTenant(ctx, tx, "nope")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing tenant: %v, want ErrNotFound", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list and fingerprint follow writes", func(t *testing.T) {
		withGamma := fingerprint(t)
		if withGamma == empty {
			t.Fatal("fingerprint did not change on create")
		}
		if _, err := put("delta", 0); err != nil {
			t.Fatal(err)
		}
		all, err := s.DashboardTenants(ctx)
		if err != nil || len(all) != 2 || all[0].Slug != "delta" || all[1].Slug != "gamma" {
			t.Fatalf("DashboardTenants = %+v, %v", all, err)
		}
		if err := del("delta", 1); err != nil {
			t.Fatal(err)
		}
		// Recreated at revision 1: only updated_at tells it apart.
		if _, err := put("delta", 0); err != nil {
			t.Fatal(err)
		}
		recreated := fingerprint(t)
		if err := del("delta", 1); err != nil {
			t.Fatal(err)
		}
		if fp := fingerprint(t); fp != withGamma || recreated == withGamma {
			t.Fatalf("fingerprints: gamma %q, recreated %q, after delete %q", withGamma, recreated, fp)
		}
	})
	resetDashboard(t, s)
}

func TestApplyConfigManagedBy(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	file := parse(t, twoTenants)
	merged, err := configfile.Merge(file, []configfile.DashboardTenant{
		{Slug: "gamma", Spec: dashboardSpec("gamma", "gamma-bot"), Revision: 1},
	}, plainOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := s.ApplyConfig(ctx, merged, "test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	row := func(t *testing.T, query string) (string, bool) {
		t.Helper()
		var managedBy string
		var enabled bool
		if err := s.owner.QueryRow(ctx, query).Scan(&managedBy, &enabled); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return managedBy, enabled
	}
	const (
		tenantQ = `SELECT managed_by, enabled FROM tenants WHERE slug = 'gamma'`
		instQ   = `SELECT managed_by, enabled FROM installations WHERE name = 'gamma-bot'`
		repoQ   = `SELECT managed_by, enabled FROM repositories WHERE name = 'gamma/one'`
	)

	t.Run("dashboard rows are dashboard-managed", func(t *testing.T) {
		for _, q := range []string{tenantQ, instQ, repoQ} {
			if by, on := row(t, q); by != "dashboard" || !on {
				t.Fatalf("%s: managed_by=%s enabled=%v", q, by, on)
			}
		}
		if by, _ := row(t, `SELECT managed_by, enabled FROM tenants WHERE slug = 'alpha'`); by != "file" {
			t.Fatalf("alpha managed_by=%s", by)
		}
	})

	t.Run("a file tenant never takes over a live dashboard one", func(t *testing.T) {
		clash := parse(t, twoTenants+`
  - slug: gamma
    installations:
      - name: gamma-file-bot
        forge: forgejo
        account: gamma
        token: { env: KRITIK_TEST_TOKEN }
        webhookSecret: { env: KRITIK_TEST_TOKEN }
`)
		err := s.ApplyConfig(ctx, clash, "test")
		if !errors.Is(err, ErrManagedBy) || !strings.Contains(err.Error(), "managed by dashboard") {
			t.Fatalf("ApplyConfig = %v, want ErrManagedBy naming the dashboard", err)
		}
		if by, on := row(t, tenantQ); by != "dashboard" || !on {
			t.Fatalf("gamma after refused apply: managed_by=%s enabled=%v", by, on)
		}
	})

	t.Run("a deleted dashboard tenant is disabled", func(t *testing.T) {
		gone, err := configfile.Merge(merged, nil, plainOpener{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyConfig(ctx, gone, "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		for _, q := range []string{tenantQ, instQ} {
			if by, on := row(t, q); by != "dashboard" || on {
				t.Fatalf("%s: managed_by=%s enabled=%v; want disabled and still dashboard", q, by, on)
			}
		}
	})

	t.Run("re-adding it re-enables it", func(t *testing.T) {
		if err := s.ApplyConfig(ctx, merged, "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		for _, q := range []string{tenantQ, instQ, repoQ} {
			if by, on := row(t, q); by != "dashboard" || !on {
				t.Fatalf("%s: managed_by=%s enabled=%v", q, by, on)
			}
		}
	})
	if err := s.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatalf("ApplyConfig cleanup: %v", err)
	}
}
