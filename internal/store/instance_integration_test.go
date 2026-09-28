//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

func resetInstanceSpec(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.owner.Exec(context.Background(), `DELETE FROM instance_config`); err != nil {
		t.Fatalf("reset instance_config: %v", err)
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

func TestInstanceSpec(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	resetInstanceSpec(t, s)
	fingerprint := func(t *testing.T) string {
		t.Helper()
		fp, err := s.InstanceSpecFingerprint(ctx)
		if err != nil {
			t.Fatalf("InstanceSpecFingerprint: %v", err)
		}
		return fp
	}
	if spec, err := s.InstanceSpec(ctx); err != nil || spec.Revision != 0 || spec.Spec != nil {
		t.Fatalf("no spec stored = %+v, %v", spec, err)
	}
	empty := fingerprint(t)
	put := func(expected int64, spec string) (int64, error) {
		var rev int64
		err := inTx(t, s, func(tx pgx.Tx) error {
			if err := LockInstanceSpec(ctx, tx); err != nil {
				return err
			}
			var err error
			rev, err = s.PutInstanceSpec(ctx, tx, json.RawMessage(spec), expected)
			return err
		})
		return rev, err
	}
	steps := []struct {
		name     string
		expected int64
		spec     string
		wantRev  int64
		wantErr  error
	}{
		{"the first write", 0, `{"polling":{"interval":"1m"}}`, 1, nil},
		{"another first write conflicts", 0, `{}`, 0, ErrSpecConflict},
		{"a write at the current revision", 1, `{"polling":{"interval":"2m"}}`, 2, nil},
		{"a write at a stale revision conflicts", 1, `{}`, 0, ErrSpecConflict},
		{"a write ahead of the stored revision conflicts", 9, `{}`, 0, ErrSpecConflict},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			rev, err := put(st.expected, st.spec)
			if !errors.Is(err, st.wantErr) || rev != st.wantRev {
				t.Fatalf("got rev %d, err %v; want rev %d, err %v", rev, err, st.wantRev, st.wantErr)
			}
		})
	}

	spec, err := s.InstanceSpec(ctx)
	if err != nil || spec.Revision != 2 {
		t.Fatalf("stored = %+v, %v", spec, err)
	}
	if decoded, err := configfile.DecodeSpec(spec.Spec); err != nil || decoded.Polling.Interval == nil || *decoded.Polling.Interval != 2*time.Minute {
		t.Fatalf("stored spec = %s, %v", spec.Spec, err)
	}
	err = inTx(t, s, func(tx pgx.Tx) error {
		in, err := InstanceSpecIn(ctx, tx)
		if err != nil {
			return err
		}
		if in.Revision != 2 {
			t.Fatalf("in tx = %+v", in)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fp := fingerprint(t); fp == empty || fp == "" {
		t.Fatalf("fingerprint %q did not change on write", fp)
	}
}

// TestRecordWebhookDelivery: a verified delivery is stamped at most once a
// minute.
func TestRecordWebhookDelivery(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := parse(t, twoAccounts)
	if err := s.ApplyConfig(ctx, f); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	in, _ := f.Connection("alpha-bot")
	at := func() *time.Time {
		var ts *time.Time
		if err := s.owner.QueryRow(ctx, `SELECT last_webhook_at FROM connections WHERE id = $1`, in.ID()).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	if _, err := s.owner.Exec(ctx, `UPDATE connections SET last_webhook_at = NULL WHERE id = $1`, in.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWebhookDelivery(ctx, in.ID()); err != nil {
		t.Fatal(err)
	}
	first := at()
	if first == nil {
		t.Fatal("no delivery recorded")
	}
	if err := s.RecordWebhookDelivery(ctx, in.ID()); err != nil {
		t.Fatal(err)
	}
	if again := at(); !again.Equal(*first) {
		t.Fatalf("a second delivery within the minute moved the stamp from %s to %s", first, again)
	}
}
