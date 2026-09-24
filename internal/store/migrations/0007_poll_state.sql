-- One row per installation: when the leader last listed its open pull
-- requests. Kept in the database so a restarted leader resumes from where
-- the previous one stopped rather than re-listing everything.
CREATE TABLE poll_state (
    installation_id uuid        PRIMARY KEY REFERENCES installations (id),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    last_polled_at  timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX poll_state_tenant_id_idx ON poll_state (tenant_id);

ALTER TABLE poll_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON poll_state
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
