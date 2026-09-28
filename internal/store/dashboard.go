package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// ErrDashboardConflict is a dashboard account write whose expected revision
// no longer matches the row: another write landed first, or a create found
// the slug already taken.
var ErrDashboardConflict = errors.New("store: dashboard account was changed by another write")

// ErrNotFound is a lookup for a row that does not exist.
var ErrNotFound = errors.New("store: not found")

// DashboardAccountMeta is who wrote a dashboard account and when. An account
// id is "" once the account is deleted.
type DashboardAccountMeta struct {
	CreatedBy string
	UpdatedBy string
	UpdatedAt time.Time
}

// dashboardExists reports whether dashboard_accounts exists: on a database
// the leader has not yet migrated there are no dashboard accounts rather
// than an error.
func (s *Store) dashboardExists(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.app.QueryRow(ctx, `SELECT to_regclass('dashboard_accounts') IS NOT NULL`).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: check dashboard accounts: %w", err)
	}
	return ok, nil
}

// DashboardAccounts returns every dashboard account, sorted by slug.
func (s *Store) DashboardAccounts(ctx context.Context) ([]configfile.DashboardAccount, error) {
	if ok, err := s.dashboardExists(ctx); err != nil || !ok {
		return nil, err
	}
	return listDashboardAccounts(ctx, s.app)
}

// DashboardAccountsIn returns every dashboard account as tx sees it, sorted
// by slug: under LockDashboardWrites, every committed write.
func DashboardAccountsIn(ctx context.Context, tx pgx.Tx) ([]configfile.DashboardAccount, error) {
	return listDashboardAccounts(ctx, tx)
}

func listDashboardAccounts(
	ctx context.Context, q interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
) ([]configfile.DashboardAccount, error) {
	rows, err := q.Query(ctx, `SELECT slug, spec, revision FROM dashboard_accounts ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: list dashboard accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (configfile.DashboardAccount, error) {
		var d configfile.DashboardAccount
		err := row.Scan(&d.Slug, &d.Spec, &d.Revision)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list dashboard accounts: %w", err)
	}
	return out, nil
}

// LockDashboardWrites serialises, until tx ends, every dashboard account
// write, so each is validated against all the others that committed
// before it: dashboard_accounts has no constraint across rows, and two
// accounts claiming one connection name would each merge on their own.
func LockDashboardWrites(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kritik:dashboard-accounts', 0))`); err != nil {
		return fmt.Errorf("store: lock dashboard accounts: %w", err)
	}
	return nil
}

// DashboardFingerprint changes whenever a dashboard account is created,
// updated or deleted, so a poll can tell cheaply whether DashboardAccounts
// would return something new. updated_at is part of it because an account
// deleted and created again starts over at revision 1.
func (s *Store) DashboardFingerprint(ctx context.Context) (string, error) {
	if ok, err := s.dashboardExists(ctx); err != nil || !ok {
		return "", err
	}
	var fp string
	err := s.app.QueryRow(ctx, `
		SELECT count(*)::text || ':' || md5(coalesce(string_agg(slug || ':' || revision || ':' || updated_at::text, ',' ORDER BY slug), ''))
		FROM dashboard_accounts`).Scan(&fp)
	if err != nil {
		return "", fmt.Errorf("store: dashboard fingerprint: %w", err)
	}
	return fp, nil
}

// PutDashboardAccount creates or replaces a dashboard account's spec in tx and
// returns its new revision. expectedRevision 0 creates the account; any other
// value replaces it only while the row is still at that revision. Either
// mismatch is ErrDashboardConflict. account is the writer's account id, ""
// for none.
func (s *Store) PutDashboardAccount(
	ctx context.Context, tx pgx.Tx, slug string, spec json.RawMessage, expectedRevision int64, account string,
) (int64, error) {
	var (
		rev int64
		err error
	)
	switch {
	case expectedRevision < 0:
		return 0, fmt.Errorf("store: dashboard account %s: expected revision %d is negative", slug, expectedRevision)
	case expectedRevision == 0:
		err = tx.QueryRow(ctx, `
			INSERT INTO dashboard_accounts (slug, spec, created_by, updated_by)
			VALUES ($1, $2, nullif($3, '')::uuid, nullif($3, '')::uuid)
			ON CONFLICT (slug) DO NOTHING
			RETURNING revision`, slug, spec, account).Scan(&rev)
	default:
		err = tx.QueryRow(ctx, `
			UPDATE dashboard_accounts SET spec = $2, revision = revision + 1, updated_by = nullif($4, '')::uuid, updated_at = now()
			WHERE slug = $1 AND revision = $3
			RETURNING revision`, slug, spec, expectedRevision, account).Scan(&rev)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store: dashboard account %s: %w", slug, ErrDashboardConflict)
	}
	if err != nil {
		return 0, fmt.Errorf("store: put dashboard account %s: %w", slug, err)
	}
	return rev, nil
}

// DeleteDashboardAccount deletes a dashboard account in tx while it is still
// at expectedRevision: ErrNotFound when there is no such account,
// ErrDashboardConflict when it has moved on. The account's rows are disabled
// by the next ApplyConfig, not deleted.
func (s *Store) DeleteDashboardAccount(ctx context.Context, tx pgx.Tx, slug string, expectedRevision int64) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		WITH deleted AS (DELETE FROM dashboard_accounts WHERE slug = $1 AND revision = $2 RETURNING slug)
		SELECT EXISTS (SELECT 1 FROM deleted)`, slug, expectedRevision).Scan(&exists)
	if err != nil {
		return fmt.Errorf("store: delete dashboard account %s: %w", slug, err)
	}
	if !exists {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dashboard_accounts WHERE slug = $1)`, slug).Scan(&exists); err != nil {
			return fmt.Errorf("store: delete dashboard account %s: %w", slug, err)
		}
		if exists {
			return fmt.Errorf("store: dashboard account %s: %w", slug, ErrDashboardConflict)
		}
		return fmt.Errorf("store: dashboard account %s: %w", slug, ErrNotFound)
	}
	// The kritik_config trigger fires on insert and update only; a delete
	// announces itself, delivered on commit like the trigger's.
	if _, err := tx.Exec(ctx, `SELECT pg_notify('kritik_config', $1)`, slug); err != nil {
		return fmt.Errorf("store: notify dashboard account %s deleted: %w", slug, err)
	}
	return nil
}

// DashboardAccount reads one dashboard account in tx, ErrNotFound when there
// is none.
func (s *Store) DashboardAccount(ctx context.Context, tx pgx.Tx, slug string) (configfile.DashboardAccount, DashboardAccountMeta, error) {
	var (
		d                    configfile.DashboardAccount
		m                    DashboardAccountMeta
		createdBy, updatedBy *string
	)
	err := tx.QueryRow(ctx, `
		SELECT slug, spec, revision, created_by, updated_by, updated_at FROM dashboard_accounts WHERE slug = $1`, slug).
		Scan(&d.Slug, &d.Spec, &d.Revision, &createdBy, &updatedBy, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, m, fmt.Errorf("store: dashboard account %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return d, m, fmt.Errorf("store: read dashboard account %s: %w", slug, err)
	}
	if createdBy != nil {
		m.CreatedBy = *createdBy
	}
	if updatedBy != nil {
		m.UpdatedBy = *updatedBy
	}
	return d, m, nil
}
