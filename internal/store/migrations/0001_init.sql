-- kritik's schema. Tenant-scoped tables carry tenant_id and a row-level
-- security policy keyed on the transaction-local setting app.tenant_id. The
-- policy normalises the setting with NULLIF because after a transaction-local
-- set_config ends the setting reads back as '' rather than NULL, and ''::uuid
-- raises. Tables a runner writes carry a second, runner_job policy keyed on
-- app.runner_job_id; PERMISSIVE policies are OR'd, so either setting opens
-- the matching rows.
--
-- The role that runs migrations owns these tables and therefore bypasses the
-- policies without BYPASSRLS. Every request and job runs as the application
-- role, which owns nothing; a runner runs as the runner role.
--
-- index_chunks, the vector table, is not here: its column dimension is
-- deployment configuration, so the leader creates it at startup (see
-- EnsureIndexSchema).

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
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- The forge's own id for a GitHub App installation, learned from the
    -- installation webhook; token minting needs it.
    external_id     bigint
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

-- body, labels ([{name, color}]) and merged feed the .kritik.yaml filter's
-- pr variable, which the worker rebuilds after the runner; body also goes
-- into the review prompt.
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
    body          text        NOT NULL DEFAULT '',
    labels        jsonb       NOT NULL DEFAULT '[]'::jsonb,
    merged        boolean     NOT NULL DEFAULT false,
    UNIQUE (repository_id, number)
);
CREATE INDEX pull_requests_tenant_id_idx ON pull_requests (tenant_id);

-- A review is one pass over one head of one pull request: a single
-- completion or an agentic loop (mode), over the whole diff or only what
-- changed since the prior review (scope). summary holds the contract's
-- summary (take and praise); skip_reason says why the repository's own
-- configuration ended it skipped.
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
    finished_at     timestamptz,
    summary         jsonb,
    mode            text        NOT NULL DEFAULT 'single' CHECK (mode IN ('single', 'agentic')),
    prior_review_id uuid        REFERENCES reviews (id),
    scope           text        NOT NULL DEFAULT 'full' CHECK (scope IN ('full', 'incremental')),
    scope_reason    text        NOT NULL DEFAULT '',
    skip_reason     text        NOT NULL DEFAULT '' CHECK (skip_reason IN ('', 'disabled', 'filtered', 'only_skipped_paths'))
);
CREATE INDEX reviews_tenant_id_idx ON reviews (tenant_id);
CREATE INDEX reviews_pull_request_idx ON reviews (pull_request_id, created_at DESC);

-- runner_runs is the record of the Kubernetes Job that prepared a review or
-- an index generation. The runner stamps heartbeat_at while it works; the
-- worker ends a run whose heartbeat has gone stale instead of waiting out
-- the Job deadline. The leader deletes each run's job-scoped Secret once
-- the run can no longer need it and stamps secret_swept_at; the partial
-- index keeps the sweep's scan to rows still pending.
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
    error              text        NOT NULL DEFAULT '',
    heartbeat_at       timestamptz,
    secret_swept_at    timestamptz
);
CREATE INDEX runner_runs_tenant_id_idx ON runner_runs (tenant_id);
CREATE INDEX runner_runs_secret_pending_idx ON runner_runs (tenant_id, created_at) WHERE secret_swept_at IS NULL;

-- context_packs is what a review's Job produced: the diff, the changed
-- paths, the context stages, .kritik.yaml and the files it and the operator
-- name as read from the merge-base tree (repo_files; repo_notes says what
-- could not be read), and for a re-review the head of the last completed
-- review when the runner could fetch it (prior_head_sha, NULL when there
-- was none or it was unreachable), the diff from it and the paths it
-- touches that no ignore glob covers.
CREATE TABLE context_packs (
    runner_run_id  uuid        PRIMARY KEY REFERENCES runner_runs (id),
    tenant_id      uuid        NOT NULL REFERENCES tenants (id),
    head_sha       text        NOT NULL,
    base_sha       text        NOT NULL,
    patch_id       text        NOT NULL,
    diff           text        NOT NULL,
    changed_paths  text[]      NOT NULL DEFAULT '{}',
    stages         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    repo_files     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    repo_notes     text[]      NOT NULL DEFAULT '{}',
    prior_head_sha text,
    delta_diff     text        NOT NULL DEFAULT '',
    delta_paths    text[]      NOT NULL DEFAULT '{}'
);
CREATE INDEX context_packs_tenant_id_idx ON context_packs (tenant_id);

-- A finding of the review contract: anchored to line, or to the range
-- line through end_line (0 for line alone), with an explanation, an
-- optional suggested fix, an optional replacement for those lines that the
-- forge offers as a one-click suggestion, and a prompt a coding agent
-- applies the fix from. fingerprint (path and normalised title) recognises
-- the same finding across reviews; posted_inline is true when an inline
-- comment for it is on the forge, posted by its own review or by an
-- earlier one with the same fingerprint.
CREATE TABLE findings (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id),
    review_id        uuid        NOT NULL REFERENCES reviews (id),
    path             text        NOT NULL,
    line             int         NOT NULL,
    severity         text        NOT NULL CHECK (severity IN ('blocking', 'important', 'nit')),
    title            text        NOT NULL,
    explanation      text        NOT NULL,
    forge_comment_id bigint,
    created_at       timestamptz NOT NULL DEFAULT now(),
    suggested_fix    text        NOT NULL DEFAULT '',
    fingerprint      text        NOT NULL DEFAULT '',
    posted_inline    boolean     NOT NULL DEFAULT false,
    end_line         int         NOT NULL DEFAULT 0,
    replacement      text        NOT NULL DEFAULT '',
    agent_prompt     text        NOT NULL DEFAULT ''
);
CREATE INDEX findings_tenant_id_idx ON findings (tenant_id);
CREATE INDEX findings_review_idx ON findings (review_id);

CREATE TABLE sticky_comments (
    pull_request_id  uuid   PRIMARY KEY REFERENCES pull_requests (id),
    tenant_id        uuid   NOT NULL REFERENCES tenants (id),
    forge_comment_id bigint NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sticky_comments_tenant_id_idx ON sticky_comments (tenant_id);

CREATE TABLE usage (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    repository_id uuid        REFERENCES repositories (id),
    review_id     uuid        REFERENCES reviews (id),
    role          text        NOT NULL CHECK (role IN ('review', 'fallback', 'embedding', 'followup')),
    model         text        NOT NULL,
    upstream      text        NOT NULL DEFAULT '',
    input_tokens  bigint      NOT NULL DEFAULT 0,
    output_tokens bigint      NOT NULL DEFAULT 0,
    cost_usd      numeric(12, 6) NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX usage_tenant_created_idx ON usage (tenant_id, created_at DESC);

-- The embedding index. index_runs is a generation of one repository's
-- index (or an incremental step of the active one); a repository points at
-- its active generation. The runner stages chunk text under its run id;
-- the worker embeds it into index_chunks.
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

-- A follow-up is one @-mention of the bot in a pull request thread and
-- what the service did about it. The comment id is unique so a redelivered
-- webhook cannot answer twice.
CREATE TABLE followups (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id),
    pull_request_id  uuid        NOT NULL REFERENCES pull_requests (id),
    comment_id       bigint      NOT NULL,
    author           text        NOT NULL DEFAULT '',
    inline           boolean     NOT NULL DEFAULT false,
    path             text        NOT NULL DEFAULT '',
    line             int         NOT NULL DEFAULT 0,
    status           text        NOT NULL CHECK (status IN ('answered', 'limited', 'ignored', 'failed')),
    reason           text        NOT NULL DEFAULT '',
    reply_comment_id bigint,
    model            text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (pull_request_id, comment_id)
);
CREATE INDEX followups_tenant_id_idx ON followups (tenant_id);
CREATE INDEX followups_pr_created_idx ON followups (pull_request_id, created_at DESC);

-- One row per installation: when the leader last listed its open pull
-- requests, so a restarted leader resumes where the previous one stopped.
CREATE TABLE poll_state (
    installation_id uuid        PRIMARY KEY REFERENCES installations (id),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    last_polled_at  timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX poll_state_tenant_id_idx ON poll_state (tenant_id);

-- An agentic review's tool loop, written by the runner after its context
-- pack: how it stopped ('skipped', with the skip reason as the error, when
-- the runner did not run it because the worker will skip the review), the
-- submitted review when it did, the tool histogram, usage and cost, and a
-- per-step timeline of tool names, duration, output bytes and tokens.
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

ALTER TABLE tenants         ENABLE ROW LEVEL SECURITY;
ALTER TABLE installations   ENABLE ROW LEVEL SECURITY;
ALTER TABLE repositories    ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_leases    ENABLE ROW LEVEL SECURITY;
ALTER TABLE pull_requests   ENABLE ROW LEVEL SECURITY;
ALTER TABLE reviews         ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_runs     ENABLE ROW LEVEL SECURITY;
ALTER TABLE context_packs   ENABLE ROW LEVEL SECURITY;
ALTER TABLE findings        ENABLE ROW LEVEL SECURITY;
ALTER TABLE sticky_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage           ENABLE ROW LEVEL SECURITY;
ALTER TABLE index_runs      ENABLE ROW LEVEL SECURITY;
ALTER TABLE index_packs     ENABLE ROW LEVEL SECURITY;
ALTER TABLE index_staging   ENABLE ROW LEVEL SECURITY;
ALTER TABLE followups       ENABLE ROW LEVEL SECURITY;
ALTER TABLE poll_state      ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_runs      ENABLE ROW LEVEL SECURITY;

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
CREATE POLICY tenant_isolation ON pull_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON reviews
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON runner_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON context_packs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON findings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON sticky_comments
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON usage
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON index_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON index_packs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON index_staging
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON followups
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON poll_state
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON agent_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The runner may update the phase of its own run and write its own packs,
-- staging rows and agent run.
CREATE POLICY runner_job ON runner_runs
    USING      (id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
CREATE POLICY runner_job ON context_packs
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
CREATE POLICY runner_job ON index_packs
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
CREATE POLICY runner_job ON index_staging
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
CREATE POLICY runner_job ON agent_runs
    USING      (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid)
    WITH CHECK (runner_run_id = NULLIF(current_setting('app.runner_job_id', true), '')::uuid);
