package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AuditEntry is one row to write to the audit log. TenantID is recorded
// only while the tenant row exists and is visible to the transaction, so a
// tenant created from the dashboard before the leader has applied it logs
// its creation against no tenant; Target still names it.
type AuditEntry struct {
	UserID   string
	TenantID string
	Action   string
	Target   string
	Detail   json.RawMessage
}

// AuditEvent is one audit log row.
type AuditEvent struct {
	ID       int64
	At       time.Time
	Actor    *User
	TenantID string
	Action   string
	Target   string
	Detail   json.RawMessage
}

// InsertAudit writes e in tx.
func InsertAudit(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	detail := e.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (user_id, tenant_id, action, target, detail)
		VALUES (nullif($1, '')::uuid, (SELECT id FROM tenants WHERE id = nullif($2, '')::uuid), $3, $4, $5)`,
		e.UserID, e.TenantID, e.Action, e.Target, detail)
	if err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	return nil
}

// ListAudit returns a page of audit events, newest first: of one tenant
// when tenantID is set, of every tenant otherwise. The cursor's ID is the
// last event's id.
func (s *Store) ListAudit(ctx context.Context, tenantID string, p Page) ([]AuditEvent, *Cursor, error) {
	if p.Limit <= 0 {
		return nil, nil, ErrPageLimit
	}
	var after *int64
	if !p.After.First() {
		n, err := strconv.ParseInt(p.After.ID, 10, 64)
		if err != nil || n <= 0 {
			return nil, nil, ErrFilter
		}
		after = &n
	}
	rows, err := s.app.Query(ctx, `SELECT e.id, e.at, e.user_id::text, coalesce(u.display_name, ''), coalesce(u.email, ''),
			coalesce(u.avatar_url, ''), coalesce(e.tenant_id::text, ''), e.action, e.target, e.detail
		FROM audit_events e LEFT JOIN users u ON u.id = e.user_id
		WHERE ($1::uuid IS NULL OR e.tenant_id = $1) AND ($2::bigint IS NULL OR e.id < $2)
		ORDER BY e.id DESC LIMIT $3`, uuidParam(tenantID), after, p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list audit events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEvent, error) {
		var (
			e      AuditEvent
			userID *string
			u      User
		)
		err := row.Scan(&e.ID, &e.At, &userID, &u.DisplayName, &u.Email, &u.AvatarURL, &e.TenantID, &e.Action, &e.Target, &e.Detail)
		if userID != nil {
			u.ID = *userID
			e.Actor = &u
		}
		return e, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: list audit events: %w", err)
	}
	items, next := paged(out, p.Limit, func(e AuditEvent) Cursor { return Cursor{ID: strconv.FormatInt(e.ID, 10)} })
	return items, next, nil
}

// LiveNonDashboard lists what a dashboard tenant writing in tx would take
// over: "slug" when the tx's tenant has a live row another origin manages,
// and each of names that is a live installation another origin manages.
// Row-level security confines the check to the tenant tx is scoped to.
func LiveNonDashboard(ctx context.Context, tx pgx.Tx, tenantID string, names []string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT 'slug' FROM tenants WHERE id = $1 AND enabled AND managed_by <> 'dashboard'
		UNION ALL
		SELECT name FROM installations WHERE name = ANY($2) AND enabled AND managed_by <> 'dashboard'`, tenantID, names)
	if err != nil {
		return nil, fmt.Errorf("store: check live rows: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: check live rows: %w", err)
	}
	return out, nil
}

// TenantRowExists reports whether the tenant has a tenants row, enabled or
// not, managed by either origin. tx must be scoped to tenantID.
func TenantRowExists(ctx context.Context, tx pgx.Tx, tenantID string) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: tenant exists: %w", err)
	}
	return ok, nil
}

// InstallationsHeldElsewhere lists each of names that an installation row
// of a tenant other than the one tx is scoped to holds, in any state.
// Row-level security hides those rows, but not the unique index on name:
// each name the tenant cannot see is probed with an insert that stops at
// that index, inside a savepoint that is always rolled back. A name that is
// free either inserts or, when the tenant has no tenants row yet, fails its
// foreign key; both mean no one holds it.
func InstallationsHeldElsewhere(ctx context.Context, tx pgx.Tx, tenantID string, names []string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT n FROM unnest($1::text[]) n WHERE NOT EXISTS (SELECT 1 FROM installations WHERE name = n)`, names)
	if err != nil {
		return nil, fmt.Errorf("store: check installation names: %w", err)
	}
	unseen, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: check installation names: %w", err)
	}
	var held []string
	for _, name := range unseen {
		taken, err := probeInstallationName(ctx, tx, tenantID, name)
		if err != nil {
			return nil, err
		}
		if taken {
			held = append(held, name)
		}
	}
	return held, nil
}

func probeInstallationName(ctx context.Context, tx pgx.Tx, tenantID, name string) (taken bool, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: probe installation %s: %w", name, err)
	}
	// The probe never keeps its row; a savepoint that fails to roll back
	// leaves tx unusable, so that is the probe's error too.
	defer func() {
		if rerr := sp.Rollback(ctx); rerr != nil {
			taken, err = false, errors.Join(err, fmt.Errorf("store: probe installation %s: roll back: %w", name, rerr))
		}
	}()
	var id string
	err = sp.QueryRow(ctx, `
		INSERT INTO installations (id, tenant_id, name, forge, managed_by)
		VALUES (gen_random_uuid(), $1, $2, 'github', 'dashboard')
		ON CONFLICT DO NOTHING RETURNING id`, tenantID, name).Scan(&id)
	switch pgErr, _ := errors.AsType[*pgconn.PgError](err); {
	case errors.Is(err, pgx.ErrNoRows):
		return true, nil
	case pgErr != nil && pgErr.Code == "23503":
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: probe installation %s: %w", name, err)
	}
	return false, nil
}
