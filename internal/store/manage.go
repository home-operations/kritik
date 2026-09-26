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

// MembershipSource is where a membership came from.
type MembershipSource string

// Membership sources, as memberships.source spells them.
const (
	SourceForge  MembershipSource = "forge"
	SourceInvite MembershipSource = "invite"
)

// Valid reports whether s is a membership source.
func (s MembershipSource) Valid() bool { return s == SourceForge || s == SourceInvite }

func (s MembershipSource) String() string { return string(s) }

// ErrInviteExists is an invite for an email that already has a pending,
// unexpired invite to the tenant.
var ErrInviteExists = errors.New("store: a pending invite for that email already exists")

// MemberGrant is one source of an account's access to a tenant.
type MemberGrant struct {
	Source MembershipSource
	Role   Role
}

// Member is an account with access to a tenant and every source of it.
type Member struct {
	Account Account
	Grants  []MemberGrant
}

// Role is the member's effective role: the highest any source gives.
func (m Member) Role() Role {
	for _, g := range m.Grants {
		if g.Role == RoleAdmin {
			return RoleAdmin
		}
	}
	return RoleMember
}

// AdminIdentity is one identity of an account that is an admin of a tenant,
// enough to tell whether the account is an operator.
type AdminIdentity struct {
	AccountID string
	Identity  SignInIdentity
}

// Invite is a pending invitation to a tenant.
type Invite struct {
	ID        string
	TenantID  string
	Email     string
	Role      Role
	CreatedBy *Account
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AuditEntry is one row to write to the audit log. TenantID is recorded
// only while the tenant row exists and is visible to the transaction, so a
// tenant created from the dashboard before the leader has applied it logs
// its creation against no tenant; Target still names it.
type AuditEntry struct {
	AccountID string
	TenantID  string
	Action    string
	Target    string
	Detail    json.RawMessage
}

// AuditEvent is one audit log row.
type AuditEvent struct {
	ID       int64
	At       time.Time
	Actor    *Account
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
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (account_id, tenant_id, action, target, detail)
		VALUES (nullif($1, '')::uuid, (SELECT id FROM tenants WHERE id = nullif($2, '')::uuid), $3, $4, $5)`,
		e.AccountID, e.TenantID, e.Action, e.Target, detail)
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
	rows, err := s.app.Query(ctx, `SELECT e.id, e.at, e.account_id::text, coalesce(a.display_name, ''), coalesce(a.email, ''),
			coalesce(a.avatar_url, ''), coalesce(e.tenant_id::text, ''), e.action, e.target, e.detail
		FROM audit_events e LEFT JOIN accounts a ON a.id = e.account_id
		WHERE ($1::uuid IS NULL OR e.tenant_id = $1) AND ($2::bigint IS NULL OR e.id < $2)
		ORDER BY e.id DESC LIMIT $3`, uuidParam(tenantID), after, p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list audit events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEvent, error) {
		var (
			e       AuditEvent
			account *string
			a       Account
		)
		err := row.Scan(&e.ID, &e.At, &account, &a.DisplayName, &a.Email, &a.AvatarURL, &e.TenantID, &e.Action, &e.Target, &e.Detail)
		if account != nil {
			a.ID = *account
			e.Actor = &a
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

func probeInstallationName(ctx context.Context, tx pgx.Tx, tenantID, name string) (bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: probe installation %s: %w", name, err)
	}
	defer func() { _ = sp.Rollback(ctx) }() // the probe never keeps its row
	var id string
	err = sp.QueryRow(ctx, `
		INSERT INTO installations (id, tenant_id, name, forge, account, credential_kind, managed_by)
		VALUES (gen_random_uuid(), $1, $2, 'github', '', 'token', 'dashboard')
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

// TenantMembers lists every account with access to the tenant, by display
// name, with each source of that access.
func TenantMembers(ctx context.Context, tx pgx.Tx, tenantID string) ([]Member, error) {
	rows, err := tx.Query(ctx, `SELECT a.id, a.display_name, a.email, a.email_verified, a.avatar_url, m.source, m.role
		FROM memberships m JOIN accounts a ON a.id = m.account_id
		WHERE m.tenant_id = $1
		ORDER BY lower(a.display_name), a.id, m.source`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: tenant members: %w", err)
	}
	var (
		out    []Member
		a      Account
		source MembershipSource
		role   Role
	)
	_, err = pgx.ForEachRow(rows, []any{&a.ID, &a.DisplayName, &a.Email, &a.EmailVerified, &a.AvatarURL, &source, &role}, func() error {
		if n := len(out); n == 0 || out[n-1].Account.ID != a.ID {
			out = append(out, Member{Account: a})
		}
		m := &out[len(out)-1]
		m.Grants = append(m.Grants, MemberGrant{Source: source, Role: role})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: tenant members: %w", err)
	}
	return out, nil
}

// MemberGrants returns the account's sources of access to the tenant.
func MemberGrants(ctx context.Context, tx pgx.Tx, tenantID, accountID string) ([]MemberGrant, error) {
	rows, err := tx.Query(ctx, `SELECT source, role FROM memberships WHERE tenant_id = $1 AND account_id = $2 ORDER BY source`,
		tenantID, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: member grants: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[MemberGrant])
	if err != nil {
		return nil, fmt.Errorf("store: member grants: %w", err)
	}
	return out, nil
}

// AdminIdentities returns the identities of every account other than
// except that is an admin of the tenant by any source, so a caller can
// tell which of them are operators. An account with no identity is
// returned with an empty one.
func AdminIdentities(ctx context.Context, tx pgx.Tx, tenantID, except string) ([]AdminIdentity, error) {
	rows, err := tx.Query(ctx, `SELECT a.id, coalesce(i.provider, ''), coalesce(i.origin, ''), coalesce(i.subject, ''),
			coalesce(i.login, ''), coalesce(i.email, ''), a.email_verified
		FROM accounts a LEFT JOIN identities i ON i.account_id = a.id
		WHERE a.id IS DISTINCT FROM nullif($2, '')::uuid
			AND a.id IN (SELECT account_id FROM memberships WHERE tenant_id = $1 AND role = 'admin')
		ORDER BY a.id`, tenantID, except)
	if err != nil {
		return nil, fmt.Errorf("store: admin identities: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminIdentity, error) {
		var x AdminIdentity
		id := &x.Identity
		err := row.Scan(&x.AccountID, &id.Provider, &id.Origin, &id.Subject, &id.Login, &id.Email, &id.EmailVerified)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: admin identities: %w", err)
	}
	return out, nil
}

// SetInviteRole changes the role of the account's invite-sourced membership
// of the tenant; ErrNotFound when it has none.
func SetInviteRole(ctx context.Context, tx pgx.Tx, tenantID, accountID string, role Role, now time.Time) error {
	if !role.Valid() {
		return fmt.Errorf("store: set invite role: invalid role %q", role)
	}
	tag, err := tx.Exec(ctx, `UPDATE memberships SET role = $3, refreshed_at = $4
		WHERE tenant_id = $1 AND account_id = $2 AND source = 'invite'`, tenantID, accountID, role, now)
	if err != nil {
		return fmt.Errorf("store: set invite role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: invite membership: %w", ErrNotFound)
	}
	return nil
}

// DeleteInviteMembership removes the account's invite-sourced membership
// of the tenant; ErrNotFound when it has none. Forge-sourced access is
// untouched: the next sign-in derives it again.
func DeleteInviteMembership(ctx context.Context, tx pgx.Tx, tenantID, accountID string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND account_id = $2 AND source = 'invite'`,
		tenantID, accountID)
	if err != nil {
		return fmt.Errorf("store: delete invite membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: invite membership: %w", ErrNotFound)
	}
	return nil
}

// PendingInvites lists the tenant's invites not yet accepted or expired,
// newest first.
func PendingInvites(ctx context.Context, tx pgx.Tx, tenantID string, now time.Time) ([]Invite, error) {
	rows, err := tx.Query(ctx, `SELECT v.id, v.tenant_id, v.email, v.role, v.created_at, v.expires_at,
			v.created_by::text, coalesce(a.display_name, ''), coalesce(a.email, ''), coalesce(a.avatar_url, '')
		FROM invites v LEFT JOIN accounts a ON a.id = v.created_by
		WHERE v.tenant_id = $1 AND v.accepted_at IS NULL AND v.expires_at > $2
		ORDER BY v.created_at DESC, v.id`, tenantID, now)
	if err != nil {
		return nil, fmt.Errorf("store: pending invites: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invite, error) {
		var (
			v  Invite
			by *string
			a  Account
		)
		err := row.Scan(&v.ID, &v.TenantID, &v.Email, &v.Role, &v.CreatedAt, &v.ExpiresAt, &by, &a.DisplayName, &a.Email, &a.AvatarURL)
		if by != nil {
			a.ID = *by
			v.CreatedBy = &a
		}
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: pending invites: %w", err)
	}
	return out, nil
}

// CreateInvite stores a pending invite, ErrInviteExists when one is
// already pending for the email. An expired invite for the email is
// dropped first: it would otherwise hold the pending slot forever.
func CreateInvite(ctx context.Context, tx pgx.Tx, v Invite, createdBy string, now time.Time) error {
	if !v.Role.Valid() {
		return fmt.Errorf("store: create invite: invalid role %q", v.Role)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invites WHERE tenant_id = $1 AND lower(email) = lower($2)
		AND accepted_at IS NULL AND expires_at <= $3`, v.TenantID, v.Email, now); err != nil {
		return fmt.Errorf("store: create invite: %w", err)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: create invite: %w", err)
	}
	defer func() { _ = sp.Rollback(ctx) }() // no-op after a successful commit
	_, err = sp.Exec(ctx, `INSERT INTO invites (id, tenant_id, email, role, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, nullif($5, '')::uuid, $6, $7)`, v.ID, v.TenantID, v.Email, v.Role, createdBy, now, v.ExpiresAt)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrInviteExists
	}
	if err != nil {
		return fmt.Errorf("store: create invite: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("store: create invite: %w", err)
	}
	return nil
}

// DeleteInvite deletes one of the tenant's pending invites and returns it;
// ErrNotFound when there is none.
func DeleteInvite(ctx context.Context, tx pgx.Tx, tenantID, id string) (Invite, error) {
	v := Invite{ID: id, TenantID: tenantID}
	err := tx.QueryRow(ctx, `DELETE FROM invites WHERE id = $1 AND tenant_id = $2 AND accepted_at IS NULL
		RETURNING email, role, created_at, expires_at`, id, tenantID).Scan(&v.Email, &v.Role, &v.CreatedAt, &v.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, fmt.Errorf("store: invite %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return v, fmt.Errorf("store: delete invite: %w", err)
	}
	return v, nil
}

// LockTenantAdmins serialises, until tx ends, every change that could take
// away one of the tenant's admins, so two admins demoting each other at
// once cannot both see the other as the admin who remains.
func LockTenantAdmins(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kritik:tenant-admins:' || $1, 0))`, tenantID); err != nil {
		return fmt.Errorf("store: lock tenant admins: %w", err)
	}
	return nil
}

// IsMemberEmail reports whether an account whose verified email is email
// already has access to the tenant, by any source.
func IsMemberEmail(ctx context.Context, tx pgx.Tx, tenantID, email string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM memberships m JOIN accounts a ON a.id = m.account_id
		WHERE m.tenant_id = $1 AND a.email_verified AND lower(a.email) = lower($2))`, tenantID, email).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: member email: %w", err)
	}
	return ok, nil
}

// DeleteTenantAccess removes every membership of and pending invite to the
// tenant. Tenant ids derive from slugs, so a tenant deleted and created
// again under the same slug would otherwise inherit the old access.
func DeleteTenantAccess(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("store: delete tenant access: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invites WHERE tenant_id = $1 AND accepted_at IS NULL`, tenantID); err != nil {
		return fmt.Errorf("store: delete tenant access: %w", err)
	}
	return nil
}
