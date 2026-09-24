CREATE TABLE findings (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id),
    review_id        uuid        NOT NULL REFERENCES reviews (id),
    path             text        NOT NULL,
    line             int         NOT NULL,
    severity         text        NOT NULL CHECK (severity IN ('info', 'warning', 'error')),
    title            text        NOT NULL,
    body             text        NOT NULL,
    forge_comment_id bigint,
    created_at       timestamptz NOT NULL DEFAULT now()
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
    role          text        NOT NULL CHECK (role IN ('review', 'fallback', 'embedding')),
    model         text        NOT NULL,
    upstream      text        NOT NULL DEFAULT '',
    input_tokens  bigint      NOT NULL DEFAULT 0,
    output_tokens bigint      NOT NULL DEFAULT 0,
    cost_usd      numeric(12, 6) NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX usage_tenant_created_idx ON usage (tenant_id, created_at DESC);

ALTER TABLE findings        ENABLE ROW LEVEL SECURITY;
ALTER TABLE sticky_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage           ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON findings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON sticky_comments
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON usage
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
