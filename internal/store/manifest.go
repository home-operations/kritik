package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppManifestTTL is how long a GitHub App manifest flow may take: GitHub's
// code expires an hour after the admin confirms the App.
const AppManifestTTL = time.Hour

// ErrAppManifest is a manifest callback whose state names no flow this
// session started, or one already claimed or expired.
var ErrAppManifest = errors.New("store: no such GitHub App registration")

// AppManifestResult is what a finished manifest flow left for the admin:
// the App it registered for connection, with its client secret sealed, or
// why it failed.
type AppManifestResult struct {
	Connection   string
	Slug         string
	ClientID     string
	ClientSecret string
	Error        string
}

// CreateAppManifest starts a manifest flow for connection, bound to the
// session whose token is sessionToken, and returns the random state that
// names it. Expired flows are swept on the way.
func (s *Store) CreateAppManifest(ctx context.Context, sessionToken, connection string, now time.Time) (string, error) {
	state := randomToken()
	if _, err := s.app.Exec(ctx, `DELETE FROM app_manifests WHERE expires_at <= $1`, now); err != nil {
		return "", fmt.Errorf("store: create App manifest: %w", err)
	}
	if _, err := s.app.Exec(ctx, `INSERT INTO app_manifests (state_hash, session_hash, connection, expires_at) VALUES ($1, $2, $3, $4)`,
		tokenHash(state), tokenHash(sessionToken), connection, now.Add(AppManifestTTL)); err != nil {
		return "", fmt.Errorf("store: create App manifest: %w", err)
	}
	return state, nil
}

// ClaimAppManifest marks the flow state names claimed by its callback and
// returns its connection, or ErrAppManifest: a flow is claimed at most
// once, only by the session that started it, and only before it expires.
func (s *Store) ClaimAppManifest(ctx context.Context, state, sessionToken string, now time.Time) (string, error) {
	if state == "" || sessionToken == "" {
		return "", ErrAppManifest
	}
	var connection string
	err := s.app.QueryRow(ctx, `UPDATE app_manifests SET claimed_at = $3
		WHERE state_hash = $1 AND session_hash = $2 AND claimed_at IS NULL AND expires_at > $3
		RETURNING connection`, tokenHash(state), tokenHash(sessionToken), now).Scan(&connection)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAppManifest
	}
	if err != nil {
		return "", fmt.Errorf("store: claim App manifest: %w", err)
	}
	return connection, nil
}

// FinishAppManifest records what the claimed flow state names came to.
func (s *Store) FinishAppManifest(ctx context.Context, state string, res AppManifestResult, now time.Time) error {
	if _, err := s.app.Exec(ctx, `UPDATE app_manifests SET finished_at = $2, app_slug = $3, client_id = $4, client_secret = $5, error = $6
		WHERE state_hash = $1`, tokenHash(state), now, res.Slug, res.ClientID, res.ClientSecret, res.Error); err != nil {
		return fmt.Errorf("store: finish App manifest: %w", err)
	}
	return nil
}

// CollectAppManifests deletes and returns the finished flows of the
// session whose token is sessionToken, oldest first, so each result is
// read once.
func (s *Store) CollectAppManifests(ctx context.Context, sessionToken string) ([]AppManifestResult, error) {
	rows, err := s.app.Query(ctx, `WITH done AS (
			DELETE FROM app_manifests WHERE session_hash = $1 AND finished_at IS NOT NULL
			RETURNING finished_at, connection, app_slug, client_id, client_secret, error
		)
		SELECT connection, app_slug, client_id, client_secret, error FROM done ORDER BY finished_at`, tokenHash(sessionToken))
	if err != nil {
		return nil, fmt.Errorf("store: collect App manifests: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AppManifestResult, error) {
		var r AppManifestResult
		err := row.Scan(&r.Connection, &r.Slug, &r.ClientID, &r.ClientSecret, &r.Error)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: collect App manifests: %w", err)
	}
	return out, nil
}
