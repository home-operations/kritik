-- A review is one pass over one head of one pull request. runner_runs is
-- the record of the Kubernetes Job that prepared it; context_packs is what
-- that Job produced. The runner role writes only rows tagged with its own
-- job id, through the runner_job policies below; the application role sees
-- its tenant's rows through the tenant policies. PERMISSIVE policies are
-- OR'd, so either setting opens the matching rows.

CREATE TABLE reviews (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    pull_request_id uuid        NOT NULL REFERENCES pull_requests (id),
    head_sha        text        NOT NULL,
    merge_base_sha  text        NOT NULL DEFAULT '',
    patch_id        text        NOT NULL DEFAULT '',
    status          text        NOT NULL CHECK (status IN ('running', 'prepared', 'completed', 'superseded', 'skipped', 'capped', 'failed')),
    trigger         text        NOT NULL DEFAULT '',
    model           text        NOT NULL DEFAULT '',
    error           text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX reviews_tenant_id_idx ON reviews (tenant_id);
CREATE INDEX reviews_pull_request_idx ON reviews (pull_request_id, created_at DESC);

CREATE TABLE runner_runs (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          uuid        NOT NULL REFERENCES tenants (id),
    review_id          uuid        REFERENCES reviews (id),
    kind               text        NOT NULL CHECK (kind IN ('review', 'index')),
    job_name           text        NOT NULL DEFAULT '',
    pod_name           text        NOT NULL DEFAULT '',
    node_name          text        NOT NULL DEFAULT '',
    phase              text        NOT NULL DEFAULT 'created',
    created_at         timestamptz NOT NULL DEFAULT now(),
    scheduled_at       timestamptz,
    started_at         timestamptz,
    finished_at        timestamptz,
    exit_code          int,
    termination_reason text        NOT NULL DEFAULT '',
    deadline_exceeded  boolean     NOT NULL DEFAULT false,
    log_tail           text        NOT NULL DEFAULT '',
    error              text        NOT NULL DEFAULT ''
);
CREATE INDEX runner_runs_tenant_id_idx ON runner_runs (tenant_id);

CREATE TABLE context_packs (
    runner_run_id uuid        PRIMARY KEY REFERENCES runner_runs (id),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    head_sha      text        NOT NULL,
    base_sha      text        NOT NULL,
    patch_id      text        NOT NULL,
    diff          text        NOT NULL,
    changed_paths text[]      NOT NULL DEFAULT '{}',
    stages        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX context_packs_tenant_id_idx ON context_packs (tenant_id);

ALTER TABLE reviews       ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_runs   ENABLE ROW LEVEL SECURITY;
ALTER TABLE context_packs ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON reviews
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON runner_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON context_packs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The runner may update the phase of its own run and write its own pack.
CREATE POLICY runner_job ON runner_runs
    USING      (id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
CREATE POLICY runner_job ON context_packs
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
