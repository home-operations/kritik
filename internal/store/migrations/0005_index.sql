-- The embedding index. index_runs is a generation of one repository's
-- index (or an incremental step of the active one); a repository points at
-- its active generation. The runner stages chunk text under its run id;
-- the worker embeds it into index_chunks, a table the leader creates at
-- the deployment's embedding dimension (see EnsureIndexSchema), since a
-- vector column's dimension is fixed at creation and the dimension is
-- deployment configuration rather than schema.

CREATE TABLE index_runs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    repository_id uuid        NOT NULL REFERENCES repositories (id),
    commit_sha    text        NOT NULL,
    base_sha      text        NOT NULL DEFAULT '',
    embed_model   text        NOT NULL,
    embed_dims    int         NOT NULL,
    mode          text        NOT NULL CHECK (mode IN ('full', 'incremental')),
    status        text        NOT NULL CHECK (status IN ('running', 'completed', 'failed', 'superseded')),
    trigger       text        NOT NULL DEFAULT '',
    chunk_count   int         NOT NULL DEFAULT 0,
    error         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);
CREATE INDEX index_runs_tenant_id_idx ON index_runs (tenant_id);
CREATE INDEX index_runs_repository_idx ON index_runs (repository_id, created_at DESC);

ALTER TABLE repositories ADD COLUMN active_index_run_id uuid REFERENCES index_runs (id);
ALTER TABLE runner_runs  ADD COLUMN index_run_id uuid REFERENCES index_runs (id);

CREATE TABLE index_packs (
    runner_run_id uuid        PRIMARY KEY REFERENCES runner_runs (id),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    commit_sha    text        NOT NULL,
    base_sha      text        NOT NULL DEFAULT '',
    mode          text        NOT NULL CHECK (mode IN ('full', 'incremental')),
    changed_paths text[]      NOT NULL DEFAULT '{}',
    chunk_count   int         NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX index_packs_tenant_id_idx ON index_packs (tenant_id);

CREATE TABLE index_staging (
    id            bigserial   PRIMARY KEY,
    runner_run_id uuid        NOT NULL REFERENCES runner_runs (id),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    path          text        NOT NULL,
    start_line    int         NOT NULL,
    end_line      int         NOT NULL,
    language      text        NOT NULL DEFAULT '',
    symbol        text        NOT NULL DEFAULT '',
    kind          text        NOT NULL DEFAULT '',
    scope         text        NOT NULL DEFAULT '',
    text          text        NOT NULL
);
CREATE INDEX index_staging_run_idx ON index_staging (runner_run_id, id);
CREATE INDEX index_staging_tenant_id_idx ON index_staging (tenant_id);

-- One row: which embedding model and dimension index_chunks was created
-- for. Owner-only writes, like config_state.
CREATE TABLE index_schema (
    id          int         PRIMARY KEY CHECK (id = 1),
    embed_model text        NOT NULL,
    embed_dims  int         NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE index_runs    ENABLE ROW LEVEL SECURITY;
ALTER TABLE index_packs   ENABLE ROW LEVEL SECURITY;
ALTER TABLE index_staging ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON index_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON index_packs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON index_staging
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY runner_job ON index_packs
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
CREATE POLICY runner_job ON index_staging
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
