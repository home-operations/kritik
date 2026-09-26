//go:build integration

package configsource

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/ingest"
	"github.com/home-operations/kritik/internal/store"
)

func testEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		AppURL: testEnv(t, "KRITIK_TEST_APP_URL"), OwnerURL: testEnv(t, "KRITIK_TEST_OWNER_URL"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "kritik_app", "kritik_runner"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func withTx(t *testing.T, st *store.Store, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.App().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func clearDashboard(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := st.DashboardTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rows {
		withTx(t, st, func(tx pgx.Tx) error { return st.DeleteDashboardTenant(ctx, tx, d.Slug, d.Revision) })
	}
}

// hook posts a Forgejo ping to installation signed with secret.
func hook(t *testing.T, srv *httptest.Server, installation, secret string) int {
	t.Helper()
	body := []byte(`{"zen":"ping"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hooks/"+installation, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", "ping")
	req.Header.Set("X-Gitea-Delivery", "d-1")
	req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

type noDispatch struct{}

func (noDispatch) Dispatch(context.Context, ingest.Request) (ingest.Outcome, error) {
	return ingest.Outcome{}, errors.New("unexpected dispatch")
}

func TestDashboardTenantEndToEnd(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	clearDashboard(t, st)
	t.Cleanup(func() { clearDashboard(t, st) })
	path := configPath(t)
	k := testKeyring(t)
	dash := dashRow(t, k, "dash", "dash-bot", 1)
	withTx(t, st, func(tx pgx.Tx) error {
		_, err := st.PutDashboardTenant(ctx, tx, dash.Slug, dash.Spec, 0, "")
		return err
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("rows without a key fail Load", func(t *testing.T) {
		_, err := (&Source{Store: st, Logger: logger}).Load(ctx, path)
		if !errors.Is(err, ErrNoDashboardKey) {
			t.Fatalf("Load = %v, want ErrNoDashboardKey", err)
		}
	})

	s := &Source{Store: st, Keyring: k, Logger: logger, Poll: 50 * time.Millisecond}
	f, err := s.Load(ctx, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tenant, ok := f.Tenant("dash")
	if !ok || !hasInstallation(f, "dash-bot") {
		t.Fatal("Current is missing the dashboard tenant")
	}
	tenantID := tenant.ID()
	tenantState := func(t *testing.T) (managedBy string, enabled bool, instManagedBy string) {
		t.Helper()
		err := st.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT t.managed_by, t.enabled, i.managed_by FROM tenants t JOIN installations i ON i.tenant_id = t.id
				WHERE t.slug = 'dash' AND i.name = 'dash-bot'`).Scan(&managedBy, &enabled, &instManagedBy)
		})
		if err != nil {
			t.Fatalf("read tenant: %v", err)
		}
		return managedBy, enabled, instManagedBy
	}

	t.Run("ApplyConfig writes dashboard-managed rows", func(t *testing.T) {
		if err := st.ApplyConfig(ctx, s.Current.Get(), "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		if by, on, inst := tenantState(t); by != "dashboard" || !on || inst != "dashboard" {
			t.Fatalf("tenant managed_by=%s enabled=%v installation managed_by=%s", by, on, inst)
		}
	})

	t.Run("the hook verifies with the sealed webhook secret", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.Handle("POST /hooks/{installation}", ingest.NewHandler(s.Current, noDispatch{}, logger))
		srv := httptest.NewServer(mux)
		defer srv.Close()
		if code := hook(t, srv, "dash-bot", "wh-dash"); code != http.StatusAccepted {
			t.Fatalf("signed with the sealed secret: status %d, want 202", code)
		}
		if code := hook(t, srv, "dash-bot", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("signed with another secret: status %d, want 401", code)
		}
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = s.Run(runCtx, path, time.Hour) }()

	t.Run("a colliding row keeps the last good snapshot", func(t *testing.T) {
		before := s.Current.Get()
		clash := dashRow(t, k, "clash", "acme-bot", 1)
		withTx(t, st, func(tx pgx.Tx) error {
			_, err := st.PutDashboardTenant(ctx, tx, clash.Slug, clash.Spec, 0, "")
			return err
		})
		waitFor(t, "LastError", func() bool { return s.LastError() != nil })
		if _, ok := errors.AsType[*configfile.MergeError](s.LastError()); !ok {
			t.Fatalf("LastError = %v, want a *configfile.MergeError", s.LastError())
		}
		if s.Current.Get() != before {
			t.Fatal("the collision replaced the snapshot")
		}
		withTx(t, st, func(tx pgx.Tx) error { return st.DeleteDashboardTenant(ctx, tx, clash.Slug, 1) })
		waitFor(t, "recovery", func() bool { return s.LastError() == nil })
	})

	t.Run("deleting the row disables the tenant on the next apply", func(t *testing.T) {
		withTx(t, st, func(tx pgx.Tx) error { return st.DeleteDashboardTenant(ctx, tx, "dash", 1) })
		waitFor(t, "dash-bot gone", func() bool { return !hasInstallation(s.Current.Get(), "dash-bot") })
		if err := st.ApplyConfig(ctx, s.Current.Get(), "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		if by, on, _ := tenantState(t); by != "dashboard" || on {
			t.Fatalf("tenant managed_by=%s enabled=%v; want disabled", by, on)
		}
	})
}
