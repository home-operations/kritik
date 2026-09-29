package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// ErrSpecConflict is an instance spec write whose expected revision no
// longer matches the stored one: another write landed first.
var ErrSpecConflict = errors.New("store: the instance spec was changed by another write")

// ErrNotFound is a lookup for a row that does not exist.
var ErrNotFound = errors.New("store: not found")

// instanceConfigExists reports whether instance_config exists: on a
// database the leader has not yet migrated there is no spec rather than an
// error.
func (s *Store) instanceConfigExists(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.app.QueryRow(ctx, `SELECT to_regclass('instance_config') IS NOT NULL`).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: check instance config: %w", err)
	}
	return ok, nil
}

// InstanceSpec returns the stored instance spec, the zero InstanceSpec
// when none has been written.
func (s *Store) InstanceSpec(ctx context.Context) (configfile.InstanceSpec, error) {
	if ok, err := s.instanceConfigExists(ctx); err != nil || !ok {
		return configfile.InstanceSpec{}, err
	}
	return readInstanceSpec(ctx, s.app)
}

// InstanceSpecIn returns the stored instance spec as tx sees it: under
// LockInstanceSpec, the latest committed write.
func InstanceSpecIn(ctx context.Context, tx pgx.Tx) (configfile.InstanceSpec, error) {
	return readInstanceSpec(ctx, tx)
}

func readInstanceSpec(
	ctx context.Context, q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
) (configfile.InstanceSpec, error) {
	var spec configfile.InstanceSpec
	err := q.QueryRow(ctx, `SELECT spec, revision FROM instance_config WHERE id = 1`).Scan(&spec.Spec, &spec.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return configfile.InstanceSpec{}, nil
	}
	if err != nil {
		return spec, fmt.Errorf("store: read instance spec: %w", err)
	}
	return spec, nil
}

// LockInstanceSpec serialises, until tx ends, every instance spec write, so
// each is validated against the one it replaces: the first write has no row
// to lock.
func LockInstanceSpec(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kritik:instance-config', 0))`); err != nil {
		return fmt.Errorf("store: lock instance spec: %w", err)
	}
	return nil
}

// InstanceSpecFingerprint changes whenever the instance spec is written, so
// a poll can tell cheaply whether InstanceSpec would return something new.
func (s *Store) InstanceSpecFingerprint(ctx context.Context) (string, error) {
	if ok, err := s.instanceConfigExists(ctx); err != nil || !ok {
		return "", err
	}
	var fp string
	err := s.app.QueryRow(ctx, `SELECT coalesce((SELECT revision::text || ':' || updated_at::text FROM instance_config WHERE id = 1), '')`).
		Scan(&fp)
	if err != nil {
		return "", fmt.Errorf("store: instance spec fingerprint: %w", err)
	}
	return fp, nil
}

// PutInstanceSpec writes the instance spec in tx while the stored one is
// still at expectedRevision, 0 for none stored, and returns the new
// revision; a mismatch is ErrSpecConflict.
func (s *Store) PutInstanceSpec(ctx context.Context, tx pgx.Tx, spec json.RawMessage, expectedRevision int64) (int64, error) {
	if expectedRevision < 0 {
		return 0, fmt.Errorf("store: instance spec: expected revision %d is negative", expectedRevision)
	}
	var (
		rev int64
		err error
	)
	if expectedRevision == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO instance_config (id, spec, revision) VALUES (1, $1, 1)
			ON CONFLICT (id) DO NOTHING RETURNING revision`, spec).Scan(&rev)
	} else {
		err = tx.QueryRow(ctx, `UPDATE instance_config
			SET spec = $1, revision = revision + 1, updated_at = now()
			WHERE id = 1 AND revision = $2 RETURNING revision`, spec, expectedRevision).Scan(&rev)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store: instance spec: %w", ErrSpecConflict)
	}
	if err != nil {
		return 0, fmt.Errorf("store: put instance spec: %w", err)
	}
	return rev, nil
}

// RecordWebhookDelivery notes that the connection's webhook delivered a
// request kritik verified, at most once a minute: the dashboard needs to
// know deliveries arrive, not to count them.
func (s *Store) RecordWebhookDelivery(ctx context.Context, connectionID string) error {
	if _, err := s.app.Exec(ctx, `UPDATE connections SET last_webhook_at = now()
		WHERE id = $1 AND (last_webhook_at IS NULL OR last_webhook_at < now() - interval '1 minute')`, connectionID); err != nil {
		return fmt.Errorf("store: record webhook delivery: %w", err)
	}
	return nil
}
