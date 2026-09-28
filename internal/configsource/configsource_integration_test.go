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

// putSpec writes spec as the next revision of the instance spec.
func putSpec(t *testing.T, st *store.Store, spec configfile.InstanceSpec) {
	t.Helper()
	ctx := context.Background()
	withTx(t, st, func(tx pgx.Tx) error {
		stored, _, err := store.InstanceSpecIn(ctx, tx)
		if err != nil {
			return err
		}
		_, err = st.PutInstanceSpec(ctx, tx, spec.Spec, stored.Revision, "")
		return err
	})
}

// hook posts a GitHub event kritik accepts and ignores to connection,
// signed with secret.
func hook(t *testing.T, srv *httptest.Server, connection, secret string) int {
	t.Helper()
	body := []byte(`{}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hooks/"+connection, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "repository")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
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

func TestInstanceSpecEndToEnd(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.App().Exec(ctx, `DELETE FROM instance_config`); err != nil {
		t.Fatal(err)
	}
	path := configPath(t)
	k := testKeyring(t)
	putSpec(t, st, specOf(t, k, 0, "dash-bot:dash"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("a spec without a key fails Load", func(t *testing.T) {
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
	account, ok := f.Account(configfile.ForgeGitHub, "dash")
	if !ok || !hasConnection(f, "dash-bot") {
		t.Fatal("Current is missing the spec's connection and its account")
	}
	accountState := func(t *testing.T) (enabled bool, connManagedBy string, connEnabled bool) {
		t.Helper()
		err := st.WithAccount(ctx, account.ID(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT a.enabled, c.managed_by, c.enabled FROM accounts a, connections c
				WHERE a.id = $1 AND c.name = 'dash-bot'`, account.ID()).Scan(&enabled, &connManagedBy, &connEnabled)
		})
		if err != nil {
			t.Fatalf("read account: %v", err)
		}
		return enabled, connManagedBy, connEnabled
	}

	t.Run("ApplyConfig writes the spec's connection and its account", func(t *testing.T) {
		if err := st.ApplyConfig(ctx, s.Current.Get(), "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		if on, by, connOn := accountState(t); !on || by != "dashboard" || !connOn {
			t.Fatalf("account enabled=%v connection managed_by=%s enabled=%v", on, by, connOn)
		}
	})

	t.Run("the hook verifies with the sealed webhook secret", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.Handle("POST /hooks/{connection}", ingest.NewHandler(s.Current, noDispatch{}, logger))
		srv := httptest.NewServer(mux)
		defer srv.Close()
		if code := hook(t, srv, "dash-bot", "wh-dash-bot"); code != http.StatusAccepted {
			t.Fatalf("signed with the sealed secret: status %d, want 202", code)
		}
		if code := hook(t, srv, "dash-bot", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("signed with another secret: status %d, want 401", code)
		}
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = s.Run(runCtx, path, time.Hour) }()

	t.Run("a spec that does not merge keeps the last good snapshot", func(t *testing.T) {
		before := s.Current.Get()
		putSpec(t, st, specOf(t, k, 0, "dash-bot:dash", "dash-bot:other"))
		waitFor(t, "LastError", func() bool { return s.LastError() != nil })
		if _, ok := errors.AsType[*configfile.MergeError](s.LastError()); !ok {
			t.Fatalf("LastError = %v, want a *configfile.MergeError", s.LastError())
		}
		if s.Current.Get() != before {
			t.Fatal("the failed merge replaced the snapshot")
		}
		putSpec(t, st, specOf(t, k, 0, "dash-bot:dash"))
		waitFor(t, "recovery", func() bool { return s.LastError() == nil })
	})

	t.Run("dropping the connection disables it and its account on the next apply", func(t *testing.T) {
		putSpec(t, st, configfile.InstanceSpec{Spec: []byte(`{}`)})
		waitFor(t, "dash-bot gone", func() bool { return !hasConnection(s.Current.Get(), "dash-bot") })
		if err := st.ApplyConfig(ctx, s.Current.Get(), "test"); err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		if on, _, connOn := accountState(t); on || connOn {
			t.Fatalf("account enabled=%v connection enabled=%v; want both disabled", on, connOn)
		}
	})
}
