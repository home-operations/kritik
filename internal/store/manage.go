package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditEntry is one row to write to the audit log. AccountID is "" for an
// instance-wide action, and is recorded only while the account row exists
// and is visible to the transaction: an account the leader has not applied
// yet logs against no account, and Target still names it.
type AuditEntry struct {
	UserID    string
	AccountID string
	Action    string
	Target    string
	Detail    json.RawMessage
}

// AuditEvent is one audit log row.
type AuditEvent struct {
	ID        int64
	At        time.Time
	Actor     *User
	AccountID string
	Action    string
	Target    string
	Detail    json.RawMessage
}

// InsertAudit writes e in tx.
func InsertAudit(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	detail := e.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (user_id, account_id, action, target, detail)
		VALUES (nullif($1, '')::uuid, (SELECT id FROM accounts WHERE id = nullif($2, '')::uuid), $3, $4, $5)`,
		e.UserID, e.AccountID, e.Action, e.Target, detail)
	if err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	return nil
}

// ListAudit returns a page of audit events, newest first: of one account
// when accountID is set, of every account otherwise. The cursor's ID is the
// last event's id.
func (s *Store) ListAudit(ctx context.Context, accountID string, p Page) ([]AuditEvent, *Cursor, error) {
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
			coalesce(u.avatar_url, ''), coalesce(e.account_id::text, ''), e.action, e.target, e.detail
		FROM audit_events e LEFT JOIN users u ON u.id = e.user_id
		WHERE ($1::uuid IS NULL OR e.account_id = $1) AND ($2::bigint IS NULL OR e.id < $2)
		ORDER BY e.id DESC LIMIT $3`, uuidParam(accountID), after, p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list audit events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEvent, error) {
		var (
			e      AuditEvent
			userID *string
			u      User
		)
		err := row.Scan(&e.ID, &e.At, &userID, &u.DisplayName, &u.Email, &u.AvatarURL, &e.AccountID, &e.Action, &e.Target, &e.Detail)
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
