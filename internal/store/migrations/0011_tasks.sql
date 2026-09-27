-- Event-driven tasks (ADR-0012). task_events keeps one delivery a task may
-- run on: its normalized header and its raw payload, which jobs reference by
-- id so their arguments stay small. It is bulky and reproducible, so the
-- leader sweeps it with the transcript retention; a run outlives its event.
CREATE TABLE task_events (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants (id),
    installation_id uuid        NOT NULL REFERENCES installations (id),
    repository_id   uuid        NOT NULL REFERENCES repositories (id),
    forge           text        NOT NULL,
    event           text        NOT NULL DEFAULT '' CHECK (event IN ('', 'issue', 'pull_request', 'comment')),
    raw_event       text        NOT NULL,
    action          text        NOT NULL DEFAULT '',
    sender          text        NOT NULL DEFAULT '',
    delivery        text        NOT NULL DEFAULT '',
    subject_kind    text        NOT NULL DEFAULT '' CHECK (subject_kind IN ('', 'issue', 'pull')),
    subject_number  int         NOT NULL DEFAULT 0,
    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    received_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX task_events_tenant_id_idx ON task_events (tenant_id);
CREATE INDEX task_events_received_at_idx ON task_events (received_at);
-- A redelivered webhook is stored, and so run, once.
CREATE UNIQUE INDEX task_events_delivery_idx ON task_events (installation_id, delivery) WHERE delivery <> '';

-- task_runs is one task run on one event: queued when it is enqueued,
-- running, then succeeded, failed or skipped, with the answer's fields and
-- the actions the model proposed, kritik applied and kritik dropped, each
-- dropped one with its reason.
CREATE TABLE task_runs (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES tenants (id),
    repository_id  uuid        NOT NULL REFERENCES repositories (id),
    task           text        NOT NULL,
    event_id       uuid        REFERENCES task_events (id) ON DELETE SET NULL,
    subject_kind   text        NOT NULL DEFAULT '' CHECK (subject_kind IN ('', 'issue', 'pull')),
    subject_number int         NOT NULL DEFAULT 0,
    trigger        text        NOT NULL DEFAULT '',
    mode           text        NOT NULL CHECK (mode IN ('single', 'agentic')),
    status         text        NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'skipped')),
    reason         text        NOT NULL DEFAULT '',
    config_sha     text        NOT NULL DEFAULT '',
    model          text        NOT NULL DEFAULT '',
    fields         jsonb,
    proposed       jsonb,
    applied        jsonb,
    dropped        jsonb,
    comment_id     bigint,
    error          text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    finished_at    timestamptz,
    UNIQUE (event_id, task)
);
CREATE INDEX task_runs_repo_created_idx ON task_runs (tenant_id, repository_id, created_at DESC);
CREATE INDEX task_runs_subject_idx ON task_runs (tenant_id, repository_id, task, subject_number, created_at);

ALTER TABLE task_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE task_runs   ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON task_events
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON task_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- A task's model call is charged and recorded like a follow-up's.
ALTER TABLE usage DROP CONSTRAINT usage_role_check;
ALTER TABLE usage ADD CONSTRAINT usage_role_check CHECK (role IN ('review', 'fallback', 'embedding', 'followup', 'task'));
ALTER TABLE model_calls DROP CONSTRAINT model_calls_kind_check;
ALTER TABLE model_calls ADD CONSTRAINT model_calls_kind_check
    CHECK (kind IN ('agent_step', 'review', 'fallback', 'followup', 'task'));
ALTER TABLE model_calls ADD COLUMN task_run_id uuid REFERENCES task_runs (id);
CREATE INDEX model_calls_task_run_idx ON model_calls (task_run_id) WHERE task_run_id IS NOT NULL;
