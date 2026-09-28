//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
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
	return json.RawMessage(`{"slug":"` + slug + `","connections":[{"name":"` + inst + `","forge":"github","accounts":["` + slug + `"],` +
		`"app":{"clientId":"Iv1.test","privateKey":{"sealed":"sealed:key"},"webhookSecret":{"sealed":"sealed:wh"}}}],"repositories":[{"name":"` + slug + `/one"}]}`)
}

func resetDashboard(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.owner.Exec(context.Background(), `DELETE FROM dashboard_accounts`); err != nil {
		t.Fatalf("reset dashboard_accounts: %v", err)
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

func TestDashboardAccounts(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	resetDashboard(t, s)
	var user string
	if err := s.app.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ('op') RETURNING id`).Scan(&user); err != nil {
		t.Fatalf("insert user: %v", err)
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
			rev, err = s.PutDashboardAccount(ctx, tx, slug, dashboardSpec(slug, slug+"-bot"), expected, user)
			return err
		})
		return rev, err
	}
	del := func(slug string, expected int64) error {
		return inTx(t, s, func(tx pgx.Tx) error { return s.DeleteDashboardAccount(ctx, tx, slug, expected) })
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
		{"update a missing account conflicts", func() (int64, error) { return put("nope", 3) }, 0, ErrDashboardConflict},
		{"delete at stale revision conflicts", func() (int64, error) { return 0, del("gamma", 1) }, 0, ErrDashboardConflict},
		{"delete a missing account", func() (int64, error) { return 0, del("nope", 1) }, 0, ErrNotFound},
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
			d, m, err := s.DashboardAccount(ctx, tx, "gamma")
			if err != nil {
				return err
			}
			if d.Slug != "gamma" || d.Revision != 2 || m.CreatedBy != user || m.UpdatedBy != user || m.UpdatedAt.IsZero() {
				t.Fatalf("got %+v %+v", d, m)
			}
			if _, err := configfile.DecodeAccount(d); err != nil {
				t.Fatalf("stored spec does not decode: %v", err)
			}
			_, _, err = s.DashboardAccount(ctx, tx, "nope")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing account: %v, want ErrNotFound", err)
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
		all, err := s.DashboardAccounts(ctx)
		if err != nil || len(all) != 2 || all[0].Slug != "delta" || all[1].Slug != "gamma" {
			t.Fatalf("DashboardAccounts = %+v, %v", all, err)
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
	file := parse(t, twoAccounts)
	merged, err := configfile.Merge(file, []configfile.DashboardAccount{
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
		accountQ = `SELECT managed_by, enabled FROM accounts WHERE slug = 'gamma'`
		instQ    = `SELECT managed_by, enabled FROM connections WHERE name = 'gamma-bot'`
		repoQ    = `SELECT managed_by, enabled FROM repositories WHERE name = 'gamma/one'`
	)

	t.Run("dashboard rows are dashboard-managed", func(t *testing.T) {
		for _, q := range []string{accountQ, instQ, repoQ} {
			if by, on := row(t, q); by != "dashboard" || !on {
				t.Fatalf("%s: managed_by=%s enabled=%v", q, by, on)
			}
		}
		if by, _ := row(t, `SELECT managed_by, enabled FROM accounts WHERE slug = 'alpha'`); by != "file" {
			t.Fatalf("alpha managed_by=%s", by)
		}
	})

	t.Run("a deleted dashboard account is disabled", func(t *testing.T) {
		gone, err := configfile.Merge(merged, nil, plainOpener{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyConfig(ctx, gone, "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		for _, q := range []string{accountQ, instQ} {
			if by, on := row(t, q); by != "dashboard" || on {
				t.Fatalf("%s: managed_by=%s enabled=%v; want disabled and still dashboard", q, by, on)
			}
		}
	})

	t.Run("re-adding it re-enables it", func(t *testing.T) {
		if err := s.ApplyConfig(ctx, merged, "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		for _, q := range []string{accountQ, instQ, repoQ} {
			if by, on := row(t, q); by != "dashboard" || !on {
				t.Fatalf("%s: managed_by=%s enabled=%v", q, by, on)
			}
		}
	})
	if err := s.ApplyConfig(ctx, file, "test"); err != nil {
		t.Fatalf("ApplyConfig cleanup: %v", err)
	}
}

// swapFileAccount is a file account declaring what dashboardSpec("swap",
// "swap-bot") does: the same slug, connection and repository.
const swapFileAccount = `
  - slug: swap
    connections:
      - name: swap-bot
        forge: github
        accounts: [swap]
        app: { clientId: Iv1.test, privateKey: { env: KRITIK_TEST_TOKEN }, webhookSecret: { env: KRITIK_TEST_TOKEN } }
    repositories:
      - name: swap/one
`

func TestApplyConfigCrossOriginReAdd(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	base := parse(t, twoAccounts)
	withFile := parse(t, twoAccounts+swapFileAccount)
	withDashboard, err := configfile.Merge(base, []configfile.DashboardAccount{
		{Slug: "swap", Spec: dashboardSpec("swap", "swap-bot"), Revision: 1},
	}, plainOpener{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	states := func(t *testing.T) []string {
		t.Helper()
		var tb, ib, rb string
		var te, ie, re bool
		err := s.owner.QueryRow(ctx, `
			SELECT t.managed_by, t.enabled, i.managed_by, i.enabled, r.managed_by, r.enabled
			FROM accounts t JOIN connections i ON i.account_id = t.id JOIN repositories r ON r.connection_id = i.id
			WHERE t.slug = 'swap' AND i.name = 'swap-bot' AND r.name = 'swap/one'`).Scan(&tb, &te, &ib, &ie, &rb, &re)
		if err != nil {
			t.Fatalf("read swap rows: %v", err)
		}
		return []string{tb + ":" + strconv.FormatBool(te), ib + ":" + strconv.FormatBool(ie), rb + ":" + strconv.FormatBool(re)}
	}
	want := func(origin string, enabled bool) []string {
		v := origin + ":" + strconv.FormatBool(enabled)
		return []string{v, v, v}
	}
	tests := []struct {
		name  string
		steps []*configfile.File
		want  []string
	}{
		{"file to dashboard, one apply apart", []*configfile.File{withFile, base, withDashboard}, want("dashboard", true)},
		{"file to dashboard in the same apply", []*configfile.File{withFile, withDashboard}, want("dashboard", true)},
		{"dashboard to file, one apply apart", []*configfile.File{withDashboard, base, withFile}, want("file", true)},
		{"dashboard to file in the same apply", []*configfile.File{withDashboard, withFile}, want("file", true)},
		{"removing the account disables its repositories", []*configfile.File{withDashboard, base}, want("dashboard", false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, f := range tt.steps {
				if err := s.ApplyConfig(ctx, f, "test"); err != nil {
					t.Fatalf("step %d: ApplyConfig: %v", i, err)
				}
			}
			if got := states(t); !slices.Equal(got, tt.want) {
				t.Fatalf("swap account, connection, repository = %v, want %v", got, tt.want)
			}
		})
	}
	if err := s.ApplyConfig(ctx, base, "test"); err != nil {
		t.Fatalf("ApplyConfig cleanup: %v", err)
	}
}
