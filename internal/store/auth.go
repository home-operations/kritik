package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Role is what a session lets its user do: administer the instance, or
// read the accounts its grant names.
type Role string

// Session roles.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Valid reports whether r is a session role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleMember }

// SessionGrant is what a sign-in allowed, fixed for the session's life: the
// role, and for a member the forge accounts they read, or every account.
type SessionGrant struct {
	Role        Role
	AllAccounts bool
	// Accounts are lowercased "<forge>/<name>" keys.
	Accounts []string
	// Key fingerprints the sign-in configuration that decided the grant; a
	// caller honours the session only while it still matches.
	Key string
}

// User is a human who has signed in to the dashboard.
type User struct {
	ID            string
	DisplayName   string
	Email         string
	EmailVerified bool
	AvatarURL     string
}

// SignInIdentity is who a sign-in provider says a human is. Provider is the
// sign-in's configured name and Origin where it pointed when the human
// signed in (GitHub, the OIDC issuer, or "local" for the local admin);
// Subject is the provider's stable id for them, unique only within that
// origin.
type SignInIdentity struct {
	Provider, Origin, Subject, Login, Email string
	EmailVerified                           bool
	DisplayName, AvatarURL                  string
}

// Session is a live dashboard session, whom it belongs to and what its
// sign-in allowed.
type Session struct {
	User     User
	Identity SignInIdentity
	Grant    SessionGrant
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

// MaxLoginStates caps the sign-ins in flight at once. Starting one needs
// no session, so without a cap anyone could grow login_states without
// bound for LoginStateTTL; far more than any real dashboard's users start
// in ten minutes.
const MaxLoginStates = 10_000

var (
	// ErrSession is a session cookie that is unknown or expired.
	ErrSession = errors.New("store: session is not valid")
	// ErrLoginState is an OAuth state that is unknown, used or expired.
	ErrLoginState = errors.New("store: login state is not valid")
	// ErrLoginStatesFull is a sign-in refused because MaxLoginStates are
	// already in flight.
	ErrLoginStatesFull = errors.New("store: too many sign-ins in flight")
)

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// RandomToken is 32 random bytes, URL-safe base64 encoded without padding:
// a session or state token, a nonce.
func RandomToken() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails
	return base64.RawURLEncoding.EncodeToString(raw)
}

// UpsertIdentity finds or creates the user behind id and refreshes its
// profile. Identities are keyed by provider, origin and subject only: an
// email seen on two providers never links their users, since either
// provider may let anyone claim any address.
func (s *Store) UpsertIdentity(ctx context.Context, id SignInIdentity) (User, error) {
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	userID, err := identityUser(ctx, tx, id)
	if err != nil {
		return User{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE identities SET login = $4 WHERE provider = $1 AND origin = $2 AND subject = $3`,
		id.Provider, id.Origin, id.Subject, id.Login); err != nil {
		return User{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	u := User{ID: userID}
	if err := tx.QueryRow(ctx, `UPDATE users SET display_name = $2, email = $3, email_verified = $4, avatar_url = $5
		WHERE id = $1 RETURNING display_name, email, email_verified, avatar_url`,
		userID, id.DisplayName, id.Email, id.EmailVerified, id.AvatarURL).
		Scan(&u.DisplayName, &u.Email, &u.EmailVerified, &u.AvatarURL); err != nil {
		return User{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("store: upsert identity: %w", err)
	}
	return u, nil
}

// identityUser returns the user id of an existing identity, or
// creates both. A concurrent first sign-in of the same identity makes the
// identity insert a no-op; the user this call created is then dropped
// and the winner's returned.
func identityUser(ctx context.Context, tx pgx.Tx, id SignInIdentity) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider = $1 AND origin = $2 AND subject = $3`,
		id.Provider, id.Origin, id.Subject).Scan(&userID)
	if err == nil {
		return userID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&userID); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO identities (provider, origin, subject, user_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, origin, subject) DO NOTHING`, id.Provider, id.Origin, id.Subject, userID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return userID, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider = $1 AND origin = $2 AND subject = $3`,
		id.Provider, id.Origin, id.Subject).Scan(&userID)
	return userID, err
}

// CreateSession stores a new session for the user, signed in through
// the provider at origin with grant g, valid until expires, and returns its
// cookie value. Only the value's SHA-256 is kept. Expired sessions are
// swept on the way.
func (s *Store) CreateSession(
	ctx context.Context, userID, provider, origin string, g SessionGrant, now, expires time.Time,
) (string, error) {
	if !g.Role.Valid() {
		return "", fmt.Errorf("store: create session: invalid role %q", g.Role)
	}
	if g.Accounts == nil {
		g.Accounts = []string{}
	}
	token := RandomToken()
	if _, err := s.app.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now); err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	if _, err := s.app.Exec(ctx, `INSERT INTO sessions
		(token_hash, user_id, provider, provider_origin, role, all_accounts, accounts, grant_key, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		tokenHash(token), userID, provider, origin, g.Role, g.AllAccounts, g.Accounts, g.Key, now, expires); err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	return token, nil
}

// LookupSession returns the unexpired session a cookie value names, or
// ErrSession.
func (s *Store) LookupSession(ctx context.Context, token string, now time.Time) (Session, error) {
	if token == "" {
		return Session{}, ErrSession
	}
	hash := tokenHash(token)
	var sess Session
	err := s.app.QueryRow(ctx, `SELECT s.user_id, s.provider, s.provider_origin,
			s.role, s.all_accounts, s.accounts, s.grant_key,
			u.display_name, u.email, u.email_verified, u.avatar_url, i.subject, i.login
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		JOIN identities i ON i.user_id = s.user_id AND i.provider = s.provider AND i.origin = s.provider_origin
		WHERE s.token_hash = $1 AND s.expires_at > $2
		ORDER BY i.created_at LIMIT 1`, hash, now).
		Scan(&sess.User.ID, &sess.Identity.Provider, &sess.Identity.Origin,
			&sess.Grant.Role, &sess.Grant.AllAccounts, &sess.Grant.Accounts, &sess.Grant.Key,
			&sess.User.DisplayName, &sess.User.Email, &sess.User.EmailVerified, &sess.User.AvatarURL,
			&sess.Identity.Subject, &sess.Identity.Login)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: look up session: %w", err)
	}
	return sess, nil
}

// SweepSessions deletes the dashboard sessions and in-flight logins that
// expired by now and returns how many it deleted.
func (s *Store) SweepSessions(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.app, func(tx pgx.Tx) error {
		for _, table := range []string{"sessions", "login_states"} {
			tag, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE expires_at <= $1`, now)
			if err != nil {
				return err
			}
			n += tag.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: sweep sessions: %w", err)
	}
	return n, nil
}

// DeleteSession ends the session a cookie value names, if any.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.app.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash(token)); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// CreateLoginState stores ls for LoginStateTTL, bound to browser, the value
// of the kritika_login cookie set in the browser starting the sign-in, and
// returns the random state parameter that names it. Only SHA-256s of the
// state and browser values are kept. Expired states are swept on the way.
func (s *Store) CreateLoginState(ctx context.Context, ls LoginState, browser string, now time.Time) (string, error) {
	if browser == "" {
		return "", errors.New("store: create login state: no browser binding")
	}
	state := RandomToken()
	if _, err := s.app.Exec(ctx, `DELETE FROM login_states WHERE expires_at <= $1`, now); err != nil {
		return "", fmt.Errorf("store: create login state: %w", err)
	}
	tag, err := s.app.Exec(ctx, `INSERT INTO login_states (state_hash, provider, nonce, pkce_verifier, return_to, expires_at, browser_hash)
		SELECT $1, $2, $3, $4, $5, $6, $7 WHERE (SELECT count(*) FROM login_states) < $8`,
		tokenHash(state), ls.Provider, ls.Nonce, ls.PKCEVerifier, ls.ReturnTo, now.Add(LoginStateTTL), tokenHash(browser), MaxLoginStates)
	if err != nil {
		return "", fmt.Errorf("store: create login state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrLoginStatesFull
	}
	return state, nil
}

// ConsumeLoginState deletes and returns the login state a state parameter
// names, so each can complete at most one sign-in, or ErrLoginState. The
// state must have been created bound to browser. A mismatched browser
// leaves the state in place: a callback replayed into another browser must
// not burn the sign-in of the browser that started it.
func (s *Store) ConsumeLoginState(ctx context.Context, state, browser string, now time.Time) (LoginState, error) {
	if state == "" || browser == "" {
		return LoginState{}, ErrLoginState
	}
	tx, err := s.app.Begin(ctx)
	if err != nil {
		return LoginState{}, fmt.Errorf("store: consume login state: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var ls LoginState
	var expires time.Time
	var bound []byte
	err = tx.QueryRow(ctx, `SELECT provider, nonce, pkce_verifier, return_to, expires_at, browser_hash
		FROM login_states WHERE state_hash = $1 FOR UPDATE`, tokenHash(state)).
		Scan(&ls.Provider, &ls.Nonce, &ls.PKCEVerifier, &ls.ReturnTo, &expires, &bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginState{}, ErrLoginState
	}
	if err != nil {
		return LoginState{}, fmt.Errorf("store: consume login state: %w", err)
	}
	if subtle.ConstantTimeCompare(bound, tokenHash(browser)) != 1 {
		return LoginState{}, ErrLoginState
	}
	if _, err := tx.Exec(ctx, `DELETE FROM login_states WHERE state_hash = $1`, tokenHash(state)); err != nil {
		return LoginState{}, fmt.Errorf("store: consume login state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LoginState{}, fmt.Errorf("store: consume login state: %w", err)
	}
	if !expires.After(now) {
		return LoginState{}, ErrLoginState
	}
	return ls, nil
}
