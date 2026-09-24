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

ALTER TABLE followups ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON followups
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE usage DROP CONSTRAINT usage_role_check;
ALTER TABLE usage ADD CONSTRAINT usage_role_check CHECK (role IN ('review', 'fallback', 'embedding', 'followup'));
