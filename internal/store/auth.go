package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Role is what a membership lets an account do on a tenant.
type Role string

// Membership roles.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Valid reports whether r is a membership role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleMember }

func (r Role) String() string { return string(r) }

// Grant is a role on one tenant.
type Grant struct {
	TenantID string
	Role     Role
}

// Account is a human who has signed in to the dashboard.
type Account struct {
	ID            string
	DisplayName   string
	Email         string
	EmailVerified bool
	AvatarURL     string
}

// SignInIdentity is who a sign-in provider says a human is. Provider is the
// sign-in's configured name, Subject its stable id for the human.
type SignInIdentity struct {
	Provider, Subject, Login, Email string
	EmailVerified                   bool
	DisplayName, AvatarURL          string
}

// Session is a live dashboard session and whom it belongs to.
type Session struct {
	Account   Account
	Identity  SignInIdentity
	ExpiresAt time.Time
}

// LoginState is one in-flight OAuth authorization request.
type LoginState struct {
	Provider     string
	Nonce        string
	PKCEVerifier string
	ReturnTo     string
}

// LoginStateTTL is how long a sign-in may take between leaving for the
// provider and coming back.
const LoginStateTTL = 10 * time.Minute

// sessionTouchInterval bounds how often a session's last_seen_at is written,
// so an active dashboard does not write on every request.
const sessionTouchInterval = time.Minute

var (
	// ErrSession is a session cookie that is unknown or expired.
	ErrSession = errors.New("store: session is not valid")
	// ErrLoginState is an OAuth state that is unknown, used or expired.
	ErrLoginState = errors.New("store: login state is not valid")
)

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// UpsertIdentity finds or creates the account behind id and refreshes its
// profile. Identities are keyed by provider and subject only: an email seen
// on two providers never links their accounts, since either provider may
// let anyone claim any address.
func (s *Store) UpsertIdentity(ctx context.Context, id SignInIdentity, now time.Time) (Account, error) {
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	accountID, err := identityAccount(ctx, tx, id)
	if err != nil {
		return Account{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE identities SET login = $3, email = $4 WHERE provider = $1 AND subject = $2`,
		id.Provider, id.Subject, id.Login, id.Email); err != nil {
		return Account{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	a := Account{ID: accountID}
	if err := tx.QueryRow(ctx, `UPDATE accounts SET display_name = $2, email = $3, email_verified = $4, avatar_url = $5, last_seen_at = $6
		WHERE id = $1 RETURNING display_name, email, email_verified, avatar_url`,
		accountID, id.DisplayName, id.Email, id.EmailVerified, id.AvatarURL, now).
		Scan(&a.DisplayName, &a.Email, &a.EmailVerified, &a.AvatarURL); err != nil {
		return Account{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	return a, nil
}

// identityAccount returns the account id of an existing identity, or
// creates both. A concurrent first sign-in of the same identity makes the
// identity insert a no-op; the account this call created is then dropped
// and the winner's returned.
func identityAccount(ctx context.Context, tx pgx.Tx, id SignInIdentity) (string, error) {
	var accountID string
	err := tx.QueryRow(ctx, `SELECT account_id FROM identities WHERE provider = $1 AND subject = $2`, id.Provider, id.Subject).Scan(&accountID)
	if err == nil {
		return accountID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&accountID); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO identities (provider, subject, account_id) VALUES ($1, $2, $3)
		ON CONFLICT (provider, subject) DO NOTHING`, id.Provider, id.Subject, accountID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return accountID, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, accountID); err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT account_id FROM identities WHERE provider = $1 AND subject = $2`, id.Provider, id.Subject).Scan(&accountID)
	return accountID, err
}

// ReplaceForgeMemberships makes grants the account's forge-sourced
// memberships, dropping any it no longer holds. A grant on a tenant the
// leader has not yet created is skipped rather than failing the sign-in; the
// next sign-in picks it up. An invite-sourced membership on the same tenant
// is left alone, so an invite outlives a lapsed forge membership.
func (s *Store) ReplaceForgeMemberships(ctx context.Context, accountID string, grants []Grant, now time.Time) error {
	for _, g := range grants {
		if !g.Role.Valid() {
			return fmt.Errorf("store: replace forge memberships: invalid role %q", g.Role)
		}
	}
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: replace forge memberships: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE account_id = $1 AND source = 'forge'`, accountID); err != nil {
		return fmt.Errorf("store: replace forge memberships: %w", err)
	}
	for _, g := range grants {
		if err := insertForgeMembership(ctx, tx, accountID, g, now); err != nil {
			return fmt.Errorf("store: replace forge memberships: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: replace forge memberships: %w", err)
	}
	return nil
}

// insertForgeMembership inserts under a savepoint, since a foreign-key
// failure would otherwise abort the whole transaction.
func insertForgeMembership(ctx context.Context, tx pgx.Tx, accountID string, g Grant, now time.Time) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sp.Rollback(ctx) }() // no-op after a successful commit
	_, err = sp.Exec(ctx, `INSERT INTO memberships (tenant_id, account_id, role, source, refreshed_at)
		VALUES ($1, $2, $3, 'forge', $4) ON CONFLICT (tenant_id, account_id) DO NOTHING`, g.TenantID, accountID, g.Role, now)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23503" {
		return nil
	}
	if err != nil {
		return err
	}
	return sp.Commit(ctx)
}

// AcceptInvites turns every pending, unexpired invite for email into an
// invite-sourced membership of the account and marks it accepted, returning
// how many it accepted. email must be one the provider verified. An invite
// never lowers an existing admin to member.
func (s *Store) AcceptInvites(ctx context.Context, accountID, email string, now time.Time) (int, error) {
	if email == "" {
		return 0, nil
	}
	tag, err := s.app.Exec(ctx, `WITH accepted AS (
			UPDATE invites SET accepted_at = $3, accepted_by = $1
			WHERE accepted_at IS NULL AND expires_at > $3 AND lower(email) = lower($2)
			RETURNING tenant_id, role
		)
		INSERT INTO memberships (tenant_id, account_id, role, source, refreshed_at)
		SELECT tenant_id, $1, role, 'invite', $3 FROM accepted
		ON CONFLICT (tenant_id, account_id) DO UPDATE SET
			role = CASE WHEN memberships.role = 'admin' THEN 'admin' ELSE EXCLUDED.role END,
			source = 'invite', refreshed_at = EXCLUDED.refreshed_at`, accountID, email, now)
	if err != nil {
		return 0, fmt.Errorf("store: accept invites: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Memberships returns the account's role on each tenant it belongs to.
func (s *Store) Memberships(ctx context.Context, accountID string) (map[string]Role, error) {
	rows, err := s.app.Query(ctx, `SELECT tenant_id, role FROM memberships WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: memberships: %w", err)
	}
	out := map[string]Role{}
	var tenantID string
	var role Role
	if _, err := pgx.ForEachRow(rows, []any{&tenantID, &role}, func() error {
		out[tenantID] = role
		return nil
	}); err != nil {
		return nil, fmt.Errorf("store: memberships: %w", err)
	}
	return out, nil
}

// CreateSession stores a new session for the account, signed in through
// provider, valid until expires, and returns its cookie value. Only the
// value's SHA-256 is kept. Expired sessions are swept on the way.
func (s *Store) CreateSession(ctx context.Context, accountID, provider string, now, expires time.Time) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	if _, err := s.app.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now); err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	if _, err := s.app.Exec(ctx, `INSERT INTO sessions (token_hash, account_id, provider, created_at, expires_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $4)`, tokenHash(token), accountID, provider, now, expires); err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	return token, nil
}

// LookupSession returns the unexpired session a cookie value names, or
// ErrSession, and records that it was seen at most once a minute.
func (s *Store) LookupSession(ctx context.Context, token string, now time.Time) (Session, error) {
	if token == "" {
		return Session{}, ErrSession
	}
	hash := tokenHash(token)
	var sess Session
	var lastSeen *time.Time
	err := s.app.QueryRow(ctx, `SELECT s.account_id, s.provider, s.expires_at, s.last_seen_at,
			a.display_name, a.email, a.email_verified, a.avatar_url, i.subject, i.login
		FROM sessions s
		JOIN accounts a ON a.id = s.account_id
		JOIN identities i ON i.account_id = s.account_id AND i.provider = s.provider
		WHERE s.token_hash = $1 AND s.expires_at > $2
		ORDER BY i.created_at LIMIT 1`, hash, now).
		Scan(&sess.Account.ID, &sess.Identity.Provider, &sess.ExpiresAt, &lastSeen,
			&sess.Account.DisplayName, &sess.Account.Email, &sess.Account.EmailVerified, &sess.Account.AvatarURL,
			&sess.Identity.Subject, &sess.Identity.Login)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: look up session: %w", err)
	}
	sess.Identity.Email = sess.Account.Email
	sess.Identity.EmailVerified = sess.Account.EmailVerified
	sess.Identity.DisplayName = sess.Account.DisplayName
	sess.Identity.AvatarURL = sess.Account.AvatarURL
	if lastSeen == nil || now.Sub(*lastSeen) >= sessionTouchInterval {
		if _, err := s.app.Exec(ctx, `WITH touched AS (
				UPDATE sessions SET last_seen_at = $2 WHERE token_hash = $1 RETURNING account_id
			)
			UPDATE accounts SET last_seen_at = $2 WHERE id IN (SELECT account_id FROM touched)`, hash, now); err != nil {
			return Session{}, fmt.Errorf("store: touch session: %w", err)
		}
	}
	return sess, nil
}

// DeleteSession ends the session a cookie value names, if any.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.app.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash(token)); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// CreateLoginState stores ls for LoginStateTTL and returns the random state
// parameter that names it. Expired states are swept on the way.
func (s *Store) CreateLoginState(ctx context.Context, ls LoginState, now time.Time) (string, error) {
	state, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("store: create login state: %w", err)
	}
	if _, err := s.app.Exec(ctx, `DELETE FROM login_states WHERE expires_at <= $1`, now); err != nil {
		return "", fmt.Errorf("store: create login state: %w", err)
	}
	if _, err := s.app.Exec(ctx, `INSERT INTO login_states (state_hash, provider, nonce, pkce_verifier, return_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenHash(state), ls.Provider, ls.Nonce, ls.PKCEVerifier, ls.ReturnTo, now.Add(LoginStateTTL)); err != nil {
		return "", fmt.Errorf("store: create login state: %w", err)
	}
	return state, nil
}

// ConsumeLoginState deletes and returns the login state a state parameter
// names, so each can complete at most one sign-in, or ErrLoginState.
func (s *Store) ConsumeLoginState(ctx context.Context, state string, now time.Time) (LoginState, error) {
	if state == "" {
		return LoginState{}, ErrLoginState
	}
	var ls LoginState
	var expires time.Time
	err := s.app.QueryRow(ctx, `DELETE FROM login_states WHERE state_hash = $1
		RETURNING provider, nonce, pkce_verifier, return_to, expires_at`, tokenHash(state)).
		Scan(&ls.Provider, &ls.Nonce, &ls.PKCEVerifier, &ls.ReturnTo, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginState{}, ErrLoginState
	}
	if err != nil {
		return LoginState{}, fmt.Errorf("store: consume login state: %w", err)
	}
	if !expires.After(now) {
		return LoginState{}, ErrLoginState
	}
	return ls, nil
}
