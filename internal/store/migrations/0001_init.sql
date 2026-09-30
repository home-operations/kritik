-- kritik's schema. Account-scoped tables carry account_id and a row-level
-- security policy keyed on the transaction-local setting app.account_id. The
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
-- index_chunks, the vector table, is not here: its column dimension is the
-- configured embedder's, so the leader creates it once one is configured
-- (see EnsureIndexSchema).

-- An account is a forge account, github/<name> (ADR-0014 §2.4). Its id
-- derives from the forge and the lowercased name, so every role can address
-- it without a lookup; it exists while a connection serves it.
CREATE TABLE accounts (
    id          uuid        PRIMARY KEY,
    forge       text        NOT NULL CHECK (forge IN ('github')),
    name        text        NOT NULL,
    enabled     boolean     NOT NULL DEFAULT true,
    disabled_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- A connection is one GitHub App serving the accounts it lists. It belongs
-- to no account, so it carries no row-level security.
CREATE TABLE connections (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text        NOT NULL UNIQUE,
    forge           text        NOT NULL CHECK (forge IN ('github')),
    -- The accounts the connection serves, as a public GitHub App
    -- installed on several organizations does.
    accounts        text[]      NOT NULL DEFAULT '{}',
    managed_by      text        NOT NULL CHECK (managed_by IN ('file', 'dashboard')),
    enabled         boolean     NOT NULL DEFAULT true,
    disabled_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- When the connection's webhook last delivered a request kritik
    -- verified, so the dashboard can tell a connection whose forge sends
    -- webhooks from one kritik only polls, and when one last arrived with
    -- no signature at all, which a GitHub App with no webhook secret sends.
    -- The listener writes each at most once a minute.
    last_webhook_at          timestamptz,
    last_unsigned_webhook_at timestamptz
);

CREATE TABLE repositories (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      uuid        NOT NULL REFERENCES accounts (id),
    name            text        NOT NULL,
    default_branch  text        NOT NULL DEFAULT '',
    managed_by      text        NOT NULL CHECK (managed_by IN ('dashboard', 'forge')),
    enabled         boolean     NOT NULL DEFAULT true,
    disabled_at     timestamptz,
    -- What the forge last said of the repository; false until it says.
    archived        boolean     NOT NULL DEFAULT false,
    fork            boolean     NOT NULL DEFAULT false,
    -- An admin's choice to review the repository or not (ADR-0019 §2.3),
    -- NULL until one is made: the configuration decides until then. A
    -- repository turned off has its index dropped once turned_at is older
    -- than retention.disabledIndexGrace.
    turned_on       boolean,
    turned_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
-- GitHub names are not case sensitive: one row per name however it is
-- spelled, which the id, derived from the lowercased name, also keys. The
-- row keeps the spelling GitHub last reported.
CREATE UNIQUE INDEX repositories_account_lower_name_key ON repositories (account_id, lower(name));
-- The repository list, in name order.
CREATE INDEX repositories_account_name_idx ON repositories (account_id, name, id);

CREATE TABLE model_leases (
    account_id uuid   NOT NULL REFERENCES accounts (id),
    model_key  text   NOT NULL,
    slot       int    NOT NULL,
    job_id     bigint,
    expires_at timestamptz,
    PRIMARY KEY (account_id, model_key, slot)
);

-- One row. Not account-scoped: written by the leader, read by every replica
-- to report configuration drift.
CREATE TABLE config_state (
    id           int  PRIMARY KEY CHECK (id = 1),
    applied_hash text NOT NULL
);

-- body, labels ([{name, color}]) and merged feed the .kritik.yaml filter's
-- pr variable, which the worker rebuilds after the runner; body also goes
-- into the review prompt.
CREATE TABLE pull_requests (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
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
    url           text        NOT NULL DEFAULT '',
    opened_at     timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    body          text        NOT NULL DEFAULT '',
    labels        jsonb       NOT NULL DEFAULT '[]'::jsonb,
    merged        boolean     NOT NULL DEFAULT false,
    -- closed_at is when the pull request was closed or merged; NULL while
    -- it is open.
    closed_at     timestamptz,
    UNIQUE (repository_id, number)
);
CREATE INDEX pull_requests_account_updated_idx ON pull_requests (account_id, updated_at DESC, id DESC);

-- users, identities, sessions and login_states are all looked up before any
-- account is known (a session cookie or an OAuth callback carries no
-- account), so, like gateway_tokens, they carry no row-level security on
-- purpose: web code enforces who may see what. A user is a person who
-- signs in to the dashboard (ADR-0009).
CREATE TABLE users (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name   text        NOT NULL DEFAULT '',
    email          text        NOT NULL DEFAULT '',
    email_verified boolean     NOT NULL DEFAULT false,
    avatar_url     text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- A review is one pass over one head of one pull request: a single
-- completion or an agentic loop (mode), over the whole diff or only what
-- changed since the prior review (scope). summary holds the contract's
-- summary (take and praise); skip_reason says why the repository's own
-- configuration ended it skipped. forge_patch_id is the patch id of a bot
-- pull request's diff as its forge reports it, so a rebase that changed
-- nothing is skipped before a runner is made for it; it is compared only
-- with other forge patch ids, since the forge's diff is not the runner's
-- byte for byte. The dashboard can cancel a running review and records
-- when; the audit log records who.
CREATE TABLE reviews (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id          uuid        NOT NULL REFERENCES accounts (id),
    pull_request_id     uuid        NOT NULL REFERENCES pull_requests (id),
    head_sha            text        NOT NULL,
    merge_base_sha      text        NOT NULL DEFAULT '',
    patch_id            text        NOT NULL DEFAULT '',
    status              text        NOT NULL
        CHECK (status IN ('running', 'prepared', 'completed', 'superseded', 'skipped', 'capped', 'failed', 'canceled')),
    trigger             text        NOT NULL DEFAULT '',
    model               text        NOT NULL DEFAULT '',
    error               text        NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    summary             jsonb,
    mode                text        NOT NULL DEFAULT 'single' CHECK (mode IN ('single', 'agentic')),
    prior_review_id     uuid        REFERENCES reviews (id),
    scope               text        NOT NULL DEFAULT 'full' CHECK (scope IN ('full', 'incremental')),
    scope_reason        text        NOT NULL DEFAULT '',
    skip_reason         text        NOT NULL DEFAULT '' CHECK (skip_reason IN ('', 'disabled', 'filtered', 'only_skipped_paths')),
    forge_patch_id      text        NOT NULL DEFAULT '',
    river_job_id        bigint,
    cancel_requested_at timestamptz
);
CREATE INDEX reviews_account_id_idx ON reviews (account_id);
CREATE INDEX reviews_pull_request_idx ON reviews (pull_request_id, created_at DESC);

-- runner_runs is the record of the Kubernetes Job that prepared a review or
-- an index generation. The runner stamps heartbeat_at while it works; the
-- worker ends a run whose heartbeat has gone stale instead of waiting out
-- the Job deadline. The leader deletes each run's job-scoped Secret once
-- the run can no longer need it and stamps secret_swept_at; the partial
-- index keeps the sweep's scan to rows still pending.
CREATE TABLE runner_runs (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id         uuid        NOT NULL REFERENCES accounts (id),
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
CREATE INDEX runner_runs_account_id_idx ON runner_runs (account_id);
CREATE INDEX runner_runs_secret_pending_idx ON runner_runs (account_id, created_at) WHERE secret_swept_at IS NULL;
-- A review's newest runner run.
CREATE INDEX runner_runs_review_created_idx ON runner_runs (review_id, created_at DESC) WHERE review_id IS NOT NULL;

-- context_packs is what a review's Job produced: the diff, the changed
-- paths, the context stages, .kritik.yaml and the files it and the operator
-- name as read from the merge-base tree (repo_files; repo_notes says what
-- could not be read), and for a re-review the head of the last completed
-- review when the runner could fetch it (prior_head_sha, NULL when there
-- was none or it was unreachable), the diff from it and the paths it
-- touches that no ignore glob covers.
CREATE TABLE context_packs (
    runner_run_id  uuid        PRIMARY KEY REFERENCES runner_runs (id),
    account_id     uuid        NOT NULL REFERENCES accounts (id),
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
CREATE INDEX context_packs_account_id_idx ON context_packs (account_id);

-- A finding of the review contract: anchored to line, or to the range
-- line through end_line (0 for line alone), with an explanation, an
-- optional suggested fix, an optional replacement for those lines that the
-- forge offers as a one-click suggestion, and a prompt a coding agent
-- applies the fix from. fingerprint (path and normalised title) recognises
-- the same finding across reviews; posted_inline is true when an inline
-- comment for it is on the forge, posted by its own review or by an
-- earlier one with the same fingerprint. rules are the ids of the review
-- rules it enforces, each one its review was given.
CREATE TABLE findings (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id       uuid        NOT NULL REFERENCES accounts (id),
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
    agent_prompt     text        NOT NULL DEFAULT '',
    -- The 👍 and 👎 on the finding's inline comment, as the poller last read
    -- them; every review that carried the comment's thread holds them.
    reactions_up     int         NOT NULL DEFAULT 0,
    reactions_down   int         NOT NULL DEFAULT 0,
    rules            text[]      NOT NULL DEFAULT '{}'
);
CREATE INDEX findings_account_id_idx ON findings (account_id);
CREATE INDEX findings_review_idx ON findings (review_id);

CREATE TABLE sticky_comments (
    pull_request_id  uuid   PRIMARY KEY REFERENCES pull_requests (id),
    account_id       uuid   NOT NULL REFERENCES accounts (id),
    forge_comment_id bigint NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sticky_comments_account_id_idx ON sticky_comments (account_id);

CREATE TABLE usage (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
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
CREATE INDEX usage_account_created_idx ON usage (account_id, created_at DESC);
-- A review's cost and a review's usage rows.
CREATE INDEX usage_review_id_idx ON usage (review_id) WHERE review_id IS NOT NULL;

-- The embedding index. index_runs is a generation of one repository's
-- index (or an incremental step of the active one); a repository points at
-- its active generation. The runner stages chunk text under its run id;
-- the worker embeds it into index_chunks.
CREATE TABLE index_runs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
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
CREATE INDEX index_runs_repository_idx ON index_runs (repository_id, created_at DESC);
CREATE INDEX index_runs_account_created_idx ON index_runs (account_id, created_at DESC, id DESC);

ALTER TABLE repositories ADD COLUMN active_index_run_id uuid REFERENCES index_runs (id);
ALTER TABLE runner_runs  ADD COLUMN index_run_id uuid REFERENCES index_runs (id);

CREATE TABLE index_packs (
    runner_run_id uuid        PRIMARY KEY REFERENCES runner_runs (id),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
    base_sha      text        NOT NULL DEFAULT '',
    mode          text        NOT NULL CHECK (mode IN ('full', 'incremental')),
    changed_paths text[]      NOT NULL DEFAULT '{}',
    chunk_count   int         NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX index_packs_account_id_idx ON index_packs (account_id);

CREATE TABLE index_staging (
    id            bigserial   PRIMARY KEY,
    runner_run_id uuid        NOT NULL REFERENCES runner_runs (id),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
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
CREATE INDEX index_staging_account_id_idx ON index_staging (account_id);

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
    account_id       uuid        NOT NULL REFERENCES accounts (id),
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
CREATE INDEX followups_pr_created_idx ON followups (pull_request_id, created_at DESC);
CREATE INDEX followups_account_created_idx ON followups (account_id, created_at DESC, id DESC);
-- A follow-up looked up by the comment it answered.
CREATE INDEX followups_account_comment_idx ON followups (account_id, comment_id);

-- One row per account: when the leader last listed its open pull
-- requests, so a restarted leader resumes where the previous one stopped.
CREATE TABLE poll_state (
    account_id     uuid        PRIMARY KEY REFERENCES accounts (id),
    last_polled_at timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- An agentic review's tool loop, written by the runner after its context
-- pack: how it stopped ('skipped', with the skip reason as the error, when
-- the runner did not run it because the worker will skip the review), the
-- submitted review when it did, the tool histogram, usage and cost, a
-- per-step timeline of tool names, duration, output bytes and tokens, and
-- the URLs its run tool gave curl (ADR-0008), which the sticky comment
-- lists as the sources consulted.
CREATE TABLE agent_runs (
    runner_run_id      uuid           PRIMARY KEY REFERENCES runner_runs (id),
    account_id         uuid           NOT NULL REFERENCES accounts (id),
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
    sources            jsonb          NOT NULL DEFAULT '[]'::jsonb,
    CHECK ((stop_reason = 'submitted') = (result IS NOT NULL))
);
CREATE INDEX agent_runs_account_id_idx ON agent_runs (account_id);

-- Per-run credentials for the worker's model gateway (ADR-0004). A token
-- is looked up by its SHA-256 before any account is known, so the table has
-- no row-level security on purpose: holding the token is the
-- authorisation, and a row reveals only the ids of the run it belongs to.
CREATE TABLE gateway_tokens (
    token_hash    bytea       PRIMARY KEY,
    runner_run_id uuid        NOT NULL REFERENCES runner_runs (id),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
    review_id     uuid        NOT NULL REFERENCES reviews (id),
    repository_id uuid        NOT NULL REFERENCES repositories (id),
    -- The provider/model references the run was granted.
    model         text        NOT NULL,
    fallback      text        NOT NULL DEFAULT '',
    budget_tokens bigint      NOT NULL CHECK (budget_tokens > 0),
    spent_tokens  bigint      NOT NULL DEFAULT 0,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX gateway_tokens_runner_run_id_idx ON gateway_tokens (runner_run_id);
CREATE INDEX gateway_tokens_expires_at_idx ON gateway_tokens (expires_at);

-- One row per sign-in identity a user has signed in with, keyed by the
-- sign-in's origin as well as its name: the OIDC issuer, or GitHub's URL.
-- Pointing a sign-in at another issuer then yields new identities instead
-- of letting a subject on the new origin take over a user from the old one.
CREATE TABLE identities (
    provider   text        NOT NULL,
    origin     text        NOT NULL DEFAULT '',
    subject    text        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    login      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, origin, subject)
);
CREATE INDEX identities_user_id_idx ON identities (user_id);

-- A session is looked up by the SHA-256 of its cookie value; the value
-- itself is never stored. It records the origin it signed in through, and
-- a session whose sign-in has since moved to another origin is no longer
-- honoured. Roles come from sign-in (ADR-0014 §2.5): role is the instance
-- role, and a member reads every account when all_accounts is set and
-- otherwise the forge accounts in accounts, lowercased as
-- "<forge>/<name>". grant_key fingerprints the sign-in configuration that
-- decided the grant, so a session outlives no change to it.
CREATE TABLE sessions (
    token_hash      bytea       PRIMARY KEY,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider        text        NOT NULL,
    provider_origin text        NOT NULL DEFAULT '',
    role            text        NOT NULL CHECK (role IN ('admin', 'member')),
    all_accounts    boolean     NOT NULL,
    accounts        text[]      NOT NULL,
    grant_key       text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL
);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- login_states holds one in-flight OAuth authorization request, keyed by
-- the SHA-256 of its state parameter, until the callback consumes it or it
-- expires. It is bound to the browser that started it: browser_hash is the
-- SHA-256 of a random value held in that browser's kritik_login cookie, so
-- a callback URL replayed into another browser (login CSRF) cannot
-- complete.
CREATE TABLE login_states (
    state_hash    bytea       PRIMARY KEY,
    provider      text        NOT NULL,
    nonce         text        NOT NULL,
    pkce_verifier text        NOT NULL,
    return_to     text        NOT NULL DEFAULT '',
    expires_at    timestamptz NOT NULL,
    browser_hash  bytea       NOT NULL
);
CREATE INDEX login_states_expires_at_idx ON login_states (expires_at);

-- audit_events records dashboard-driven actions across every account, so an
-- instance admin can read it without an account context; no RLS.
CREATE TABLE audit_events (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    user_id    uuid        REFERENCES users (id) ON DELETE SET NULL,
    account_id uuid        REFERENCES accounts (id),
    action     text        NOT NULL,
    target     text        NOT NULL DEFAULT '',
    detail     jsonb       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_events_account_id_idx ON audit_events (account_id, id DESC);

-- model_calls is account content (row-level security applies, unlike the
-- tables above): one row per model call the gateway made, whether an
-- agent's step, a plain review, a fallback, or a followup reply, kept for
-- the dashboard's transcript view and cost accounting. A NULL system or
-- tools means "unchanged from the previous row of the same run/review",
-- so a long agent run doesn't repeat an unchanging system prompt or tool
-- list on every step.
--
-- messages_end, messages_sha, system_sha, tools_sha and run_bytes are the
-- state an agent step's row leaves for the next step of its run
-- (internal/transcript): the request's message count and cumulative
-- message hash, the hashes of the system prompt and tool list, and the
-- bytes the run has recorded through this row. The next step's row is a
-- delta against it, and a request whose prefix no longer matches it is
-- recorded whole. Only agent steps read it back.
CREATE TABLE model_calls (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id          uuid        NOT NULL REFERENCES accounts (id),
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
    created_at          timestamptz NOT NULL DEFAULT now(),
    messages_end        int         NOT NULL DEFAULT 0,
    messages_sha        bytea,
    system_sha          bytea,
    tools_sha           bytea,
    run_bytes           bigint      NOT NULL DEFAULT 0
);
CREATE INDEX model_calls_review_step_idx ON model_calls (review_id, step);
CREATE INDEX model_calls_runner_run_step_idx ON model_calls (runner_run_id, step);
CREATE INDEX model_calls_account_created_idx ON model_calls (account_id, created_at);
-- The retention sweep deletes by age across every account; a follow-up's
-- transcript is looked up by the comment it answered.
CREATE INDEX model_calls_created_at_idx ON model_calls (created_at);
CREATE INDEX model_calls_followup_comment_idx ON model_calls (followup_comment_id) WHERE followup_comment_id IS NOT NULL;

ALTER TABLE accounts        ENABLE ROW LEVEL SECURITY;
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
ALTER TABLE model_calls     ENABLE ROW LEVEL SECURITY;

CREATE POLICY account_isolation ON accounts
    USING      (id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON repositories
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON model_leases
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON pull_requests
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON reviews
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON runner_runs
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON context_packs
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON findings
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON sticky_comments
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON usage
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON index_runs
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON index_packs
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON index_staging
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON followups
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON poll_state
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON agent_runs
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);
CREATE POLICY account_isolation ON model_calls
    USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
    WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid);

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
        'account_id', row_json ->> 'account_id',
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
