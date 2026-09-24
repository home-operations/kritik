-- The forge's own id for a GitHub App installation, learned from the
-- installation webhook; token minting needs it.
ALTER TABLE installations ADD COLUMN external_id bigint;

CREATE TABLE pull_requests (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    repository_id uuid        NOT NULL REFERENCES repositories (id),
    number        int         NOT NULL,
    title         text        NOT NULL DEFAULT '',
    author        text        NOT NULL DEFAULT '',
    author_is_bot boolean     NOT NULL DEFAULT false,
    draft         boolean     NOT NULL DEFAULT false,
    fork          boolean     NOT NULL DEFAULT false,
    state         text        NOT NULL DEFAULT 'open',
    head_ref      text        NOT NULL DEFAULT '',
    head_sha      text        NOT NULL,
    base_ref      text        NOT NULL DEFAULT '',
    base_sha      text        NOT NULL DEFAULT '',
    url           text        NOT NULL DEFAULT '',
    opened_at     timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, number)
);
CREATE INDEX pull_requests_tenant_id_idx ON pull_requests (tenant_id);

ALTER TABLE pull_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pull_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
