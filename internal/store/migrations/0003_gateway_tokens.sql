-- Per-run credentials for the worker's model gateway (ADR-0004). A token
-- is looked up by its SHA-256 before any tenant is known, so the table has
-- no row-level security on purpose: holding the token is the
-- authorisation, and a row reveals only the ids of the run it belongs to.
CREATE TABLE gateway_tokens (
    token_hash    bytea       PRIMARY KEY,
    runner_run_id uuid        NOT NULL REFERENCES runner_runs (id),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    review_id     uuid        NOT NULL REFERENCES reviews (id),
    repository_id uuid        NOT NULL REFERENCES repositories (id),
    -- The provider/model references the run was granted.
    model         text        NOT NULL,
    fallback      text        NOT NULL DEFAULT '',
    budget_tokens bigint      NOT NULL CHECK (budget_tokens > 0),
    spent_tokens  bigint      NOT NULL DEFAULT 0,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX gateway_tokens_runner_run_id_idx ON gateway_tokens (runner_run_id);
CREATE INDEX gateway_tokens_expires_at_idx ON gateway_tokens (expires_at);
