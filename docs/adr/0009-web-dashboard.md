# ADR-0009: the web dashboard

- **Status:** Proposed
- **Date:** 2026-09-25
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.17 and
  [ADR-0001](0001-kritik-pr-review-service.md) §2.14, the deferred v2
  dashboard sketch in both: this ADR is that dashboard, built.
- **Authors:** perfectra1n.

> Scope: the `web` role, sign-in and membership, dashboard-managed tenants
> and their credentials, live updates, model-conversation capture, and the
> re-run, cancel and reindex actions. It does not change how the ingest,
> worker or poller roles review a pull request, only how they learn about
> dashboard-managed configuration and the new actions they now serve.

## 1. Context

kritik has no UI. Its state lives only in Postgres and in forge comments.
ADR-0002 §2.17 and ADR-0001 §2.14 deferred a dashboard to v2 and had v1 do
three things so it could be additive: Postgres is the source of truth even
though the file is the only writer, usage and review history are recorded
from day one, and the shared-instance App stays possible through
environment variables. This ADR is that v2, covering the whole of
ADR-0002 §2.17: dashboard-managed tenants, installations and repositories
with envelope-encrypted credentials; roles; an audit log; and three
operational actions for tenant admins — re-run a review, cancel a running
one, and trigger a full reindex — plus a fourth capability neither ADR
sketched: recording the full model conversation, every turn, every tool
call with its arguments, every tool result and the raw output.

Exploration before this ADR found:

- **The conversation is not recorded today.** `agent_runs.timeline` holds
  only step index, tool names, duration, bytes and usage
  (`internal/runner/agentic.go`). The single-shot
  `model.Structured.Complete` path throws the `StepResponse` away
  (`internal/model/structured.go`).
- **The gateway is the right place to capture it.** Every agentic step
  passes through the gateway's `chat` function (`internal/worker/gateway.go`),
  carrying the whole conversation as a `model.StepRequest`, and the
  `GatewayGrant` already carries the run and review IDs.
- **The runtime reads config only from the in-memory `*configfile.File`**,
  served through `configfile.Current`. The jsonb `settings` columns are
  write-only, so a tenant that exists only as a database row is invisible
  to ingest, the worker, the gateway and the poller: dashboard management
  means merging file rows and dashboard rows into the `File` that feeds
  `Current`.
- **The `web` role must not get the owner DSN** (ADR-0002 §2.17); it
  connects as the app role, which reads tenant data only through
  `WithTenant`, the same as every other non-owner role (ADR-0002 §2.4).
- **Nothing existed to build on:** no `LISTEN`/`NOTIFY`, no crypto library,
  no re-run, cancel or reindex function, an `@`-mention runs a follow-up
  rather than a re-run, River's `ReviewArgs` uniqueness blocks a re-run of
  the same head, and there is no `canceled` review status.

## 2. Decision

### 2.1 Stack and the `web` role

The dashboard uses konflate's frontend stack exactly, as ADR-0001 §2.14
already specified: Svelte 5 with runes, Vite, Tailwind 4, TypeScript,
Geist and Geist Mono, `@mdi/js` and `simple-icons` icons, and Playwright
for end-to-end tests, with no component library. It builds into
`internal/web/dist` and is served with `go:embed`. The hash router, store,
theme, keyboard and command-palette patterns are copied from konflate
rather than redesigned.

A new `--role web` serves it, also included in `all`. It listens on
`KRITIK_WEB_ADDR` (default `:8083`), with `KRITIK_WEB_URL` as the external
URL for OAuth redirects. It uses the app DSN only, and holds an
insert-only River client, the same shape as ingest's.

### 2.2 Live updates: server-sent events, not a websocket

`GET /api/events` is fed by Postgres `LISTEN`/`NOTIFY`: a trigger calls
`pg_notify('kritik_events', {tenant_id, kind, id, review_id})` on status
and phase changes, never on heartbeats. The payload carries IDs only; the
server filters by the caller's tenants, and the client re-fetches the
full state for whatever the notification named.

A websocket was rejected because updates only ever flow one way, server
to browser: nothing in the dashboard needs a client-to-server realtime
channel, since every action is an ordinary POST. `EventSource` adds no
client dependency; the client only adds a jittered backoff to its
reconnects, and the server a comment heartbeat so idle proxies keep the
stream open. Every stream opens with a `resync` event, and the client
treats every (re)open as one, so nothing published while it was
disconnected is missed. Shipping IDs rather than state also keeps a
notification from leaking content ahead of the per-tenant filter and a
freshly authorized fetch — a guarantee a websocket that pushed whole rows
would have to reimplement per message.

### 2.3 Sign-in and operators: the config file, not the environment

Sign-in and operator identity live under a new top-level `web:` key in the
config file, with secrets held as `SecretRef`s like every other
credential: `web.signIn[]` (`{name, type: oidc|github|forgejo,
issuer|host, clientId, clientSecret, scopes}`), `web.operators[]`
(`"<provider>:<login>"` for a forge provider, `"<provider>:<sub>"` for
OIDC, or `"email:<verified email>"`), and `web.sessionTTL` (default 12h).
Providers themselves stay file-only; a dashboard tenant may only reference
providers and models the file already declares.

The alternative was environment variables, which is how ADR-0002 §2.17
and ADR-0001 §2.14 originally sketched it: "an environment-configured
allowlist" and "a key from the environment." Every other piece of
instance configuration in kritik already has exactly one source of truth,
the config file, hot-reloaded and validated as a whole. A list of
provider objects and an operator allowlist would need their own
delimiter-and-parsing convention as environment variables, and a second
configuration channel next to the file is one more place for drift to
hide. Keeping sign-in and operator identity in the file also means the
same audit trail that governs which tenants exist, git history, governs
who can sign in as an operator.

### 2.4 Membership

Forge sign-in (a GitHub OAuth app, or Forgejo OAuth2) is checked at every
login against the tenant's installations on the same host: the user is a
member if they are the tenant's personal account or a member of its
organization, and an admin if they are an organization owner or admin.
The result is cached in `memberships` with `source=forge`. OIDC membership
works by invitation: an invite names a verified email and a role; inviting
an email that already has a membership on the tenant is refused with a
`409` `already_member` rather than creating a second grant. An account can
hold a `memberships` row per source (forge and invite) on the same tenant,
and its effective role is the highest of the two (`Member.Role`).
Operators are a file allowlist that can see and do everything, and are the
only way to create a tenant.

### 2.5 Dashboard tenant storage: sealed refs, merged into the file's snapshot

A dashboard-managed tenant is one row in `dashboard_tenants(slug, spec
jsonb, revision, updated_by, updated_at)`. `spec` is a `configfile.Tenant`
in JSON, with credentials held as `SecretRef{Sealed: "..."}`, a new third
ref kind that only dashboard input may use. `configfile.Merge(file, dash,
opener)` reuses `resolve`, `compileFilter` and `validate` to fold these
rows into the same `File` every runtime component already reads through
`configfile.Current`.

The alternative was teaching each runtime component — ingest, the worker,
the gateway and the poller — to read the already-existing but
currently-write-only jsonb `settings` columns directly. That trades one
merge function for four components each learning a second configuration
source and its own reconciliation order. Merging into the same `File`
type instead gets a dashboard tenant validation, CEL filter compilation
and secret resolution for free, since `Merge` reuses `resolve`,
`compileFilter` and `validate` rather than duplicating them.

`dashboard_tenants` has no row-level security: every replica needs the
whole set to build its snapshot, and the credentials in it are already
sealed. This is the same reasoning ADR-0004 §2.2 gives for
`gateway_tokens` having none.

### 2.6 Instance-level authorization tables: no row-level security, by design

`accounts`, `identities`, `sessions`, `memberships`, `invites`,
`audit_events` and `dashboard_tenants` carry no row-level security. They
must be readable before any tenant is known — checking who signed in, and
which tenants they may see, happens before a tenant is in scope at all —
so the web code enforces access itself. Every tenant-content read still
goes through `WithTenant` and its row-level security policy (ADR-0002
§2.4).

Row-level security partitions by tenant, but an account or a session
is not tenant data: it is instance data a login has to resolve before a
tenant identity exists to filter by, the same situation `gateway_tokens`
is in (ADR-0004 §2.2). Confining this exception to the auth surface, while
every tenant-content table keeps the row-level security ADR-0002 §2.4
describes, keeps the no-RLS list small and each entry justified the same
way.

### 2.7 Cookies and CSRF

A session cookie holds a random 256-bit value, stored server-side only as
its SHA-256, never the raw token: the server only ever needs to verify a
presented value against the hash, so a database read alone can't hand out
a usable session. Cookies are `HttpOnly; SameSite=Lax`, and `Secure`
whenever `KRITIK_WEB_URL` is https (it is dropped only for a plain-http
URL, such as a local run). Over https their names carry the `__Host-`
prefix when the dashboard is served at the root, binding them to exactly
that host so a sibling subdomain cannot plant one, and `__Secure-` under a
path.

Every mutating request must carry a matching `Origin` or a
`Sec-Fetch-Site: same-origin` header, plus an `X-Kritik: 1` header. A
cross-site form submission cannot add a custom header, so requiring one
forces the browser into a CORS preflight, and the preflight only succeeds
if the server's CORS policy allows the calling origin — closing the
classic simple-request CSRF path without a token to generate, store or
compare.

### 2.8 Transcript storage: deltas, not full requests

A new `model_calls` table, with tenant row-level security, holds one row
per model call: agentic gateway steps, single-shot review and fallback
calls, and follow-ups. An agentic step stores only its new messages: each
row keeps `messages_from`, the index of its first new message, and the
messages from there on; `system` and `tools` are stored only when their
hash changes. If a request's prefix doesn't match what is already
recorded, the whole request is stored with `messages_from=0`.

The alternative, storing the full request on every step, is quadratic in a
run's step count: an agentic run's request is the whole conversation so
far, and it grows every step, so storing it whole each time multiplies
that growth by however many steps the review runs. Storing only the delta
keeps cost proportional to what changed, with prefix-mismatch as a
well-defined fallback for the case a delta can't be computed cleanly.

A tool result over 64 KiB is cut to 64 KiB with a marker; a row is capped
at 1 MiB; a run is capped at 16 MiB, after which only the response and
usage are kept. Everything is passed through `maskProvider` and the
egress credentials mask before it is written, the same masking the log
tail already gets. "Raw output" means the model's text plus its tool-call
input exactly as returned; the vendor's wire JSON is not kept, which
bounds what "raw" means to kritik's own contract rather than
provider-specific framing.

### 2.9 Re-run: a `Request` field, not bypassing uniqueness

`ReviewArgs` gains a unique-tagged `Request string`. The usual triggers
leave it empty, so River's existing deduplication is unchanged; a manual
re-run sets a UUID with `Trigger: "manual"`.

The alternative, loosening `ReviewArgs`' uniqueness constraint so a re-run
could re-enqueue the same head, would weaken the guarantee for every
trigger, not just the manual one: that constraint is what stops a webhook
retry or a race from producing a duplicate review. Tagging the args with a
per-request value instead keeps the existing guarantee for triggers that
need it, and gives only the one trigger that explicitly wants a duplicate
a narrow way to opt out. It still requires confirming the worker does not
skip a head it has already reviewed; where it does, `manual` forces the
review through.

### 2.10 Cancel: a new status, not deleting a job out from under a run

`reviews.river_job_id` is set by `start`. `reviews.cancel_requested_at`
and `canceled_by` are set by the web handler, which then calls River's
`Client.JobCancelTx` (River 0.47). A new `canceled` status lets the
worker's cancel path check `cancel_requested_at` and finish the review as
`canceled` rather than `failed`; job deletion and lease release reuse the
existing supersede path in `internal/worker/supervise.go`.

The alternative was canceling by deleting the Job directly from outside
the worker, or reusing `failed` for it. The worker already has a
well-tested supersede path for ending a run early and releasing its
lease, so reusing it keeps run-ending logic in one place instead of adding
a second way for a review's Job to disappear. A distinct `canceled` status
also keeps `failed` meaning what it already means, that the review did
not finish because of an error, rather than overloading it with "an admin
stopped this on purpose," which matters to anyone reading failure rates
later.

### 2.11 Reindex: a forced full path

`IndexArgs` gains `Full bool`; the worker skips the incremental path in
`internal/worker/index.go` when it is set. A reindex is enqueued with
`CommitSHA: ""` and `Trigger: "reindex"`. An empty `CommitSHA` is already
what distinguishes "no specific commit" from an incremental update tied
to one, so the flag rides the signal the incremental path already
branches on instead of adding a parallel condition.

### 2.12 File-versus-dashboard collisions: file wins, the write fails

File-managed objects are read-only in the UI, and a dashboard repository
can only be added under a dashboard-managed tenant. A slug or
installation-name collision with a file-managed tenant is rejected: the
web write that would cause it gets a `409` `slug_taken` carrying the path
of the error. A colliding file edit, instead, is logged, the last good
snapshot stays live, and the drift gauge rises. `ApplyConfig`
(`internal/store/configsync.go`) writes `managed_by` from each tenant's
origin, and never takes over a row with a different `managed_by`.

Git is already the source of truth for anything a file declares
(ADR-0002 §2.6). Letting a dashboard write silently shadow or take over a
file-managed row would make the file's own declared state depend on
dashboard timing; rejecting the collision at the point of write, rather
than resolving it with a merge order, keeps the ambiguity visible to
whoever caused it instead of picking a winner silently.

### 2.13 Credentials are write-only in the UI

The UI shows whether a secret is set, never its value. The webhook
secret can be generated on the server; the UI then shows it exactly once,
with the hook path, for the operator to copy into the forge, and never
again. A sealed value is already opaque once encrypted (§2.5,
§3), so never round-tripping a plaintext credential back to the browser
removes a class of exposure, a browser history entry, a copy-paste into
the wrong place, for no loss of function: an operator who needs to change
a credential simply supplies a new one.

### 2.14 `web.dashboardForgeHosts`: bounding which forge hosts a dashboard tenant may reach

A new `web.dashboardForgeHosts []string` config key holds an
operator-set allowlist of forge hostnames a dashboard-managed tenant's
installations may reference: plain hostnames only, no scheme or path.
When it is empty, the default is `github.com` plus every host already
used by the file's own installations; a non-empty list replaces that
default rather than extending it. `configfile.Merge` rejects a dashboard
installation naming a host outside the allowlist.

Every installation host is already allowed as runner egress: `EgressRules`
scopes a run's network access to the hosts its installations name.
Without this allowlist, a dashboard tenant admin could add an installation
on an arbitrary forge host and thereby widen the runner pod's egress
allowlist beyond anything an operator declared in the file. Defaulting to
the file's own hosts plus `github.com` keeps the common case, one forge
already declared, working with no configuration, while still giving an
operator who wants a narrower or wider set of reachable hosts a way to say
so.

### 2.15 Models, forks, runner, limits, agent, mode and incremental stay operator-only on a dashboard tenant

A dashboard tenant's `models`, `forks`, `runner` and `limits` fields, and
a dashboard-managed repository's `agent`, `mode` and `incremental` fields,
can be set only by an instance operator, never by a tenant admin, through
the web API; a tenant-admin request that changes one of them is rejected
with a `422`. An operator may still set any of them for any tenant.

`models` picks which model, and so which price, every review pays for;
`forks` whether pull requests from forks, whose authors the tenant does not
control, are reviewed at all; `limits` bounds what a tenant may spend and
`runner` selects the pod a review runs in; `agent` chooses which agent
reviews the pull request, `mode` how much of the result it may act on, and
`incremental` how much of a pull request a re-review may skip re-covering.
Letting a tenant admin change any of the seven would let one tenant
unilaterally raise its own cost, its exposure to untrusted code, or its own
runner's privilege past whatever the operator sized the instance for when
the tenant was created.

## 3. Security model

| Role              | Scope                                                                                                                                                                         |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Instance operator | File allowlist. Sees and administers every tenant; the only way to create a tenant or set a tenant's `runner`, `limits`, `agent`, `mode`, `incremental`.                      |
| Tenant admin      | State-changing endpoints for their tenant(s), except `models`, `forks`, `runner`, `limits`, and repository `agent`/`mode`/`incremental` (§2.15). Every write is audit-logged. |
| Tenant member     | Read access to their tenant's own content.                                                                                                                                    |

Sessions, cookies and CSRF are as described in §2.7. Credentials are
sealed with envelope encryption from `internal/sealbox` (§2.5): each
secret gets a random 256-bit data key and AES-256-GCM, and the data key is
itself wrapped with AES-GCM under a key-encryption key from
`KRITIK_DASHBOARD_KEY` (32 bytes base64, or `_FILE`); `KRITIK_DASHBOARD_
OLD_KEYS` lists older keys, used only to decrypt, and each sealed value
records its key ID, the first 8 bytes of the SHA-256 of the key. Every
non-runner role needs the key once dashboard rows exist; startup fails
without it. `model_calls` rows pass through `maskProvider` and the egress
credentials mask before they are written (§2.8). A dashboard tenant's
installations are further bounded to `web.dashboardForgeHosts` (§2.14), so
a tenant admin cannot use the dashboard to widen runner egress past what
an operator allows.

**What web can never do:**

- **The owner DSN.** `web` connects as the app role only, reading tenant
  data through `WithTenant` like every other non-owner role (ADR-0002
  §2.4). An instance operator's cross-tenant view still runs as the app
  role, iterating the tenants the operator may see, one tenant setting per
  query; the owner DSN is never given to `web`.
- **The cluster API.** `web` never talks to Kubernetes. ADR-0001 §2.14
  already drew this boundary for the dashboard, "the `web` role keeps zero
  cluster permissions," because runner history, phases and log tails live
  in Postgres. `web` has no need to read a Job or a pod directly, and
  inherits that boundary for the same reason.

## 4. Data retention

`retention.transcripts` (default 30d) governs how long `model_calls` rows
are kept, swept by the leader. Sessions are expired and removed by the
same leader loop on their own TTL cycle, independent of transcript
retention. A `model_calls` row is additionally bounded in size regardless
of age, by the per-row and per-run caps in §2.8.

## 5. Consequences

- kritik gains a UI without a second source of truth for tenant
  configuration: every runtime component keeps reading
  `configfile.Current`, now fed by a merge of file and dashboard rows.
- The full model conversation becomes inspectable after the fact, for
  both agentic and single-shot reviews, at a storage cost proportional to
  what changed each step rather than to the conversation's whole length.
- A tenant declared only in the file remains the only kind that can
  survive a database wipe and rebuild from git; a dashboard-managed
  tenant's existence and credentials live only in Postgres.
- The `web` role adds one more listener, one more Service, and one more
  OAuth/OIDC integration surface to operate. Sign-in and operator identity
  move into the config file, so rotating an OAuth client secret or adding
  an operator is a file change like any other, not a redeploy with new
  environment variables.
- Re-run, cancel and reindex are each backed by a database row, a
  `Request` tag, a cancel timestamp, a `Full` flag, rather than a side
  channel, so they show up in the same history a webhook-triggered review
  does.

## 6. Deferred

- Whether the dashboard may edit what the file manages stays a v2
  question. The default answer is no, so that git stays the source of
  truth for anything declared in git.
- Rotating the key-encryption key needs a job that re-seals every sealed
  value under the new key. `KRITIK_DASHBOARD_OLD_KEYS` covers reading old
  ciphertext in the meantime, but the re-seal job itself is deferred.
