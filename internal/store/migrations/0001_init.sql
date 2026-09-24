-- Tenant-scoped tables carry tenant_id and a row-level security policy keyed
-- on the transaction-local setting app.tenant_id. The policy normalises the
-- setting with NULLIF because after a transaction-local set_config ends the
-- setting reads back as '' rather than NULL, and ''::uuid raises.
--
-- The role that runs migrations owns these tables and therefore bypasses the
-- policies without BYPASSRLS. Every request and job runs as the application
-- role, which owns nothing.

CREATE TABLE tenants (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text        NOT NULL UNIQUE,
    managed_by  text        NOT NULL CHECK (managed_by IN ('file', 'dashboard')),
    enabled     boolean     NOT NULL DEFAULT true,
    disabled_at timestamptz,
    settings    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE installations (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    name            text        NOT NULL UNIQUE,
    forge           text        NOT NULL CHECK (forge IN ('github', 'gitlab', 'forgejo')),
    host            text        NOT NULL DEFAULT '',
    account         text        NOT NULL,
    credential_kind text        NOT NULL CHECK (credential_kind IN ('app', 'token')),
    managed_by      text        NOT NULL CHECK (managed_by IN ('file', 'dashboard')),
    enabled         boolean     NOT NULL DEFAULT true,
    disabled_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX installations_tenant_id_idx ON installations (tenant_id);

CREATE TABLE repositories (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    installation_id uuid        NOT NULL REFERENCES installations (id),
    name            text        NOT NULL,
    default_branch  text        NOT NULL DEFAULT '',
    settings        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    managed_by      text        NOT NULL CHECK (managed_by IN ('file', 'dashboard', 'forge')),
    enabled         boolean     NOT NULL DEFAULT true,
    disabled_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_id, name)
);
CREATE INDEX repositories_tenant_id_idx ON repositories (tenant_id);

CREATE TABLE model_leases (
    tenant_id  uuid   NOT NULL REFERENCES tenants (id),
    model_key  text   NOT NULL,
    slot       int    NOT NULL,
    job_id     bigint,
    expires_at timestamptz,
    PRIMARY KEY (tenant_id, model_key, slot)
);

-- One row. Not tenant-scoped: written by the leader, read by every replica
-- to report configuration drift.
CREATE TABLE config_state (
    id           int         PRIMARY KEY CHECK (id = 1),
    applied_hash text        NOT NULL,
    applied_at   timestamptz NOT NULL DEFAULT now(),
    leader       text        NOT NULL
);

ALTER TABLE tenants       ENABLE ROW LEVEL SECURITY;
ALTER TABLE installations ENABLE ROW LEVEL SECURITY;
ALTER TABLE repositories  ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_leases  ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants
    USING      (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON installations
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON repositories
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON model_leases
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
