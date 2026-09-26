-- The web dashboard (ADR-0009): human accounts, forge/OAuth identities,
-- sessions, tenant membership and invites, an audit log, dashboard-managed
-- tenant specs, per-call model transcripts, and the LISTEN/NOTIFY plumbing
-- the dashboard's live views read from instead of polling.

-- accounts, identities, sessions and login_states are all looked up before
-- any tenant is known (a session cookie or an OAuth callback carries no
-- tenant), so, like gateway_tokens, they carry no row-level security on
-- purpose: web code enforces who may see what.
CREATE TABLE accounts (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name   text        NOT NULL DEFAULT '',
    email          text        NOT NULL DEFAULT '',
    email_verified boolean     NOT NULL DEFAULT false,
    avatar_url     text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz
);

-- One row per forge/OAuth identity a human has signed in with; several
-- identities may point at the same account (linking is out of scope here).
CREATE TABLE identities (
    provider   text        NOT NULL,
    subject    text        NOT NULL,
    account_id uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    login      text        NOT NULL DEFAULT '',
    email      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);
CREATE INDEX identities_account_id_idx ON identities (account_id);

-- A session is looked up by the SHA-256 of its cookie value; the value
-- itself is never stored.
CREATE TABLE sessions (
    token_hash   bytea       PRIMARY KEY,
    account_id   uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    provider     text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz
);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_account_id_idx ON sessions (account_id);

-- login_states holds one in-flight OAuth authorization request, keyed by
-- the SHA-256 of its state parameter, until the callback consumes it or it
-- expires.
CREATE TABLE login_states (
    state_hash    bytea       PRIMARY KEY,
    provider      text        NOT NULL,
    nonce         text        NOT NULL,
    pkce_verifier text        NOT NULL,
    return_to     text        NOT NULL DEFAULT '',
    expires_at    timestamptz NOT NULL
);

-- memberships mirrors who may act on a tenant: refreshed from the forge on
-- sign-in ("forge") or granted by an accepted invite ("invite"). No RLS: a
-- membership must be readable to decide which tenant a session may act on
-- in the first place.
CREATE TABLE memberships (
    tenant_id     uuid        NOT NULL REFERENCES tenants (id),
    account_id    uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    role          text        NOT NULL CHECK (role IN ('admin', 'member')),
    source        text        NOT NULL CHECK (source IN ('forge', 'invite')),
    refreshed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, account_id)
);
CREATE INDEX memberships_account_id_idx ON memberships (account_id);

-- invites are looked up by their id before the invited account exists, so
-- they carry no RLS either; a pending invite is unique per tenant+email.
CREATE TABLE invites (
    id           uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL REFERENCES tenants (id),
    email        text        NOT NULL,
    role         text        NOT NULL CHECK (role IN ('admin', 'member')),
    created_by   uuid        REFERENCES accounts (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    accepted_by  uuid        REFERENCES accounts (id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX invites_pending_email_idx ON invites (tenant_id, lower(email)) WHERE accepted_at IS NULL;

-- audit_events records dashboard-driven actions across every tenant, so an
-- instance admin can read it without a tenant context; no RLS.
CREATE TABLE audit_events (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    account_id uuid        REFERENCES accounts (id) ON DELETE SET NULL,
    tenant_id  uuid        REFERENCES tenants (id),
    action     text        NOT NULL,
    target     text        NOT NULL DEFAULT '',
    detail     jsonb       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_events_tenant_at_idx ON audit_events (tenant_id, at DESC);

-- dashboard_tenants holds a dashboard-authored tenant spec pending or
-- already applied by ApplyConfig, keyed by slug rather than tenant_id: the
-- tenants row itself is created later, by ApplyConfig, so a foreign key to
-- tenants(id) is not possible here. No RLS: it is read before the tenant it
-- describes exists.
CREATE TABLE dashboard_tenants (
    slug       text        PRIMARY KEY,
    spec       jsonb       NOT NULL,
    revision   bigint      NOT NULL DEFAULT 1,
    created_by uuid        REFERENCES accounts (id) ON DELETE SET NULL,
    updated_by uuid        REFERENCES accounts (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- model_calls is tenant content (row-level security applies, unlike the
-- tables above): one row per model call the gateway made, whether an
-- agent's step, a plain review, a fallback, or a followup reply, kept for
-- the dashboard's transcript view and cost accounting. A NULL system or
-- tools means "unchanged from the previous row of the same run/review",
-- so a long agent run doesn't repeat an unchanging system prompt or tool
-- list on every step.
CREATE TABLE model_calls (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants (id),
    review_id           uuid        REFERENCES reviews (id),
    runner_run_id       uuid        REFERENCES runner_runs (id),
    followup_comment_id bigint,
    kind                text        NOT NULL CHECK (kind IN ('agent_step', 'review', 'fallback', 'followup')),
    step                int         NOT NULL DEFAULT 0,
    model               text        NOT NULL DEFAULT '',
    upstream            text        NOT NULL DEFAULT '',
    system              text,
    tools               jsonb,
    messages_from       int         NOT NULL DEFAULT 0,
    messages            jsonb       NOT NULL DEFAULT '[]'::jsonb,
    response            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    stop_reason         text        NOT NULL DEFAULT '',
    input_tokens        bigint      NOT NULL DEFAULT 0,
    cache_read_tokens   bigint      NOT NULL DEFAULT 0,
    cache_write_tokens  bigint      NOT NULL DEFAULT 0,
    output_tokens       bigint      NOT NULL DEFAULT 0,
    cost_usd            numeric(12, 6) NOT NULL DEFAULT 0,
    duration_ms         int         NOT NULL DEFAULT 0,
    error               text        NOT NULL DEFAULT '',
    truncated           boolean     NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX model_calls_review_step_idx ON model_calls (review_id, step);
CREATE INDEX model_calls_runner_run_step_idx ON model_calls (runner_run_id, step);
CREATE INDEX model_calls_tenant_created_idx ON model_calls (tenant_id, created_at);

ALTER TABLE model_calls ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON model_calls
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The dashboard can cancel a running review and records who did and when;
-- 'canceled' joins the terminal statuses a review may reach.
ALTER TABLE reviews
    ADD COLUMN river_job_id        bigint,
    ADD COLUMN cancel_requested_at timestamptz,
    ADD COLUMN canceled_by         uuid REFERENCES accounts (id) ON DELETE SET NULL;

ALTER TABLE reviews DROP CONSTRAINT reviews_status_check;
ALTER TABLE reviews ADD CONSTRAINT reviews_status_check
    CHECK (status IN ('running', 'prepared', 'completed', 'superseded', 'skipped', 'capped', 'failed', 'canceled'));

-- kritik_notify_event publishes one row's change on the kritik_events
-- channel for the dashboard's live views: the kind is baked into the
-- trigger via TG_ARGV[0], and the id and (where the row has one) review_id
-- are read generically through jsonb so one function serves every table,
-- including ones with no review_id column at all (to_jsonb(NEW) ->> 'x'
-- yields SQL NULL for a column that isn't there). No SECURITY DEFINER is
-- needed: pg_notify requires no privilege beyond reading NEW, so this also
-- fires correctly when the writer is the runner role.
CREATE FUNCTION kritik_notify_event() RETURNS trigger AS $$
DECLARE
    row_json jsonb := to_jsonb(NEW);
BEGIN
    PERFORM pg_notify('kritik_events', jsonb_build_object(
        'tenant_id', row_json ->> 'tenant_id',
        'kind', TG_ARGV[0],
        'id', row_json ->> 'id',
        'review_id', row_json ->> 'review_id'
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- kritik_notify_review, kritik_notify_runner_run and kritik_notify_index_run
-- are split into separate INSERT/UPDATE triggers rather than one combined
-- "INSERT OR UPDATE ... WHEN (TG_OP = 'INSERT' OR ...)" trigger: a trigger's
-- WHEN clause is parsed as a plain boolean expression over OLD/NEW, and
-- TG_OP (a PL/pgSQL variable available only inside the trigger function
-- body) is not a resolvable column there, so it fails with "column tg_op
-- does not exist". An unconditional INSERT trigger plus a column-comparing
-- UPDATE trigger has the same effect without referencing TG_OP.
CREATE TRIGGER kritik_notify_review_insert
    AFTER INSERT ON reviews
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_event('review');

CREATE TRIGGER kritik_notify_review_update
    AFTER UPDATE ON reviews
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION kritik_notify_event('review');

CREATE TRIGGER kritik_notify_runner_run_insert
    AFTER INSERT ON runner_runs
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_event('runner_run');

CREATE TRIGGER kritik_notify_runner_run_update
    AFTER UPDATE ON runner_runs
    FOR EACH ROW
    WHEN (OLD.phase IS DISTINCT FROM NEW.phase)
    EXECUTE FUNCTION kritik_notify_event('runner_run');

CREATE TRIGGER kritik_notify_index_run_insert
    AFTER INSERT ON index_runs
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_event('index_run');

CREATE TRIGGER kritik_notify_index_run_update
    AFTER UPDATE ON index_runs
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION kritik_notify_event('index_run');

CREATE TRIGGER kritik_notify_followup
    AFTER INSERT OR UPDATE ON followups
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_event('followup');

CREATE TRIGGER kritik_notify_model_call
    AFTER INSERT ON model_calls
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_event('model_call');

-- kritik_notify_config publishes the tenant slug whenever a dashboard-edited
-- tenant spec changes, so the leader can re-apply it without polling.
CREATE FUNCTION kritik_notify_config() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('kritik_config', NEW.slug);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kritik_notify_dashboard_tenant
    AFTER INSERT OR UPDATE ON dashboard_tenants
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_config();
