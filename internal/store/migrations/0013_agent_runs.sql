-- An agentic review's tool loop, written by the runner after its context
-- pack: how it stopped ('skipped', with the skip reason as the error, when
-- the runner did not run it because the worker will skip the review), the submitted review when it did, the tool
-- histogram, usage and cost, and a per-step timeline of tool names,
-- duration, output bytes and tokens.
CREATE TABLE agent_runs (
    runner_run_id      uuid           PRIMARY KEY REFERENCES runner_runs (id),
    tenant_id          uuid           NOT NULL REFERENCES tenants (id),
    stop_reason        text           NOT NULL
        CHECK (stop_reason IN ('submitted', 'max_steps', 'budget', 'no_submit', 'canceled', 'error', 'skipped')),
    result             jsonb,
    steps              int            NOT NULL DEFAULT 0,
    tool_calls         jsonb          NOT NULL DEFAULT '{}'::jsonb,
    timeline           jsonb          NOT NULL DEFAULT '[]'::jsonb,
    input_tokens       bigint         NOT NULL DEFAULT 0,
    cache_read_tokens  bigint         NOT NULL DEFAULT 0,
    cache_write_tokens bigint         NOT NULL DEFAULT 0,
    output_tokens      bigint         NOT NULL DEFAULT 0,
    cost_usd           numeric(12, 6) NOT NULL DEFAULT 0,
    model              text           NOT NULL,
    error              text           NOT NULL DEFAULT '',
    created_at         timestamptz    NOT NULL DEFAULT now(),
    CHECK ((stop_reason = 'submitted') = (result IS NOT NULL))
);
CREATE INDEX agent_runs_tenant_id_idx ON agent_runs (tenant_id);

ALTER TABLE agent_runs ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON agent_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- The runner writes the row for its own run only.
CREATE POLICY runner_job ON agent_runs
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
