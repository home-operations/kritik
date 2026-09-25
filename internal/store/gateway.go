package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// gatewayTokenPrefix marks a run token, so a leaked one is recognisable.
const gatewayTokenPrefix = "krk_"

// ErrGatewayToken is a run token that is unknown, revoked or expired.
var ErrGatewayToken = errors.New("store: gateway token is not valid")

// GatewayGrant is what a run token lets its bearer do at the model gateway.
type GatewayGrant struct {
	RunID, TenantID, ReviewID, RepositoryID string
	// Model and Fallback are the provider/model references the run may
	// call; Fallback may be empty.
	Model, Fallback string
	// Budget is the tokens the run may spend, Spent what it has.
	Budget, Spent int64
}

func gatewayTokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// MintGatewayToken stores a new run token for g, valid until expires, and
// returns it. Only its SHA-256 is kept.
func (s *Store) MintGatewayToken(ctx context.Context, g GatewayGrant, expires time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("store: mint gateway token: %w", err)
	}
	token := gatewayTokenPrefix + hex.EncodeToString(raw)
	_, err := s.app.Exec(ctx, `INSERT INTO gateway_tokens
		(token_hash, runner_run_id, tenant_id, review_id, repository_id, model, fallback, budget_tokens, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		gatewayTokenHash(token), g.RunID, g.TenantID, g.ReviewID, g.RepositoryID, g.Model, g.Fallback, g.Budget, expires)
	if err != nil {
		return "", fmt.Errorf("store: mint gateway token: %w", err)
	}
	return token, nil
}

// LookupGatewayToken returns the grant of an unexpired token, or
// ErrGatewayToken.
func (s *Store) LookupGatewayToken(ctx context.Context, token string) (GatewayGrant, error) {
	if !strings.HasPrefix(token, gatewayTokenPrefix) {
		return GatewayGrant{}, ErrGatewayToken
	}
	var g GatewayGrant
	err := s.app.QueryRow(ctx, `SELECT runner_run_id, tenant_id, review_id, repository_id, model, fallback, budget_tokens, spent_tokens
		FROM gateway_tokens WHERE token_hash = $1 AND expires_at > now()`, gatewayTokenHash(token)).
		Scan(&g.RunID, &g.TenantID, &g.ReviewID, &g.RepositoryID, &g.Model, &g.Fallback, &g.Budget, &g.Spent)
	if errors.Is(err, pgx.ErrNoRows) {
		return GatewayGrant{}, ErrGatewayToken
	}
	if err != nil {
		return GatewayGrant{}, fmt.Errorf("store: look up gateway token: %w", err)
	}
	return g, nil
}

// ChargeGatewayToken adds tokens to what the token's run has spent.
func (s *Store) ChargeGatewayToken(ctx context.Context, token string, tokens int64) error {
	if _, err := s.app.Exec(ctx, `UPDATE gateway_tokens SET spent_tokens = spent_tokens + $2 WHERE token_hash = $1`,
		gatewayTokenHash(token), tokens); err != nil {
		return fmt.Errorf("store: charge gateway token: %w", err)
	}
	return nil
}

// RevokeGatewayTokens deletes the run's tokens, and with them every token
// that has expired, so a worker that died before revoking leaves nothing
// behind for long.
func (s *Store) RevokeGatewayTokens(ctx context.Context, runID string) error {
	if _, err := s.app.Exec(ctx, `DELETE FROM gateway_tokens WHERE runner_run_id = $1 OR expires_at <= now()`, runID); err != nil {
		return fmt.Errorf("store: revoke gateway tokens: %w", err)
	}
	return nil
}
