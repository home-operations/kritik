# ADR-0002: kritik, a multi-tenant AI pull request review service

- **Status:** Proposed
- **Date:** 2026-09-24
- **Supersedes:** [ADR-0001](0001-kritik-pr-review-service.md), kept unchanged as historical input.
- **Amended by:** [ADR-0003](0003-forgejo-agentic-review.md) (Forgejo,
  providers, the contract, `.kritik.yaml`, agentic mode, the worker–runner
  protocol), [ADR-0004](0004-model-gateway.md) (the worker as model
  gateway; no provider key in a runner pod),
  [ADR-0007](0007-vectorchord.md) (the vector index is VectorChord's
  `vchordrq`, replacing the pgvector HNSW of §2.8 and §2.9) and
  [ADR-0009](0009-web-dashboard.md) (the v2 dashboard of §2.17, built).
- **Authors:** perfectra1n (this ADR, as published at
  [gist 6a67dd03](https://gist.github.com/perfectra1n/6a67dd0362ea9d7afd24a4e917028581));
  rebased onto ADR-0001's final revision by onedr0p. The rebase adds the
  runner pod model (§2.3, §2.9, §2.10), the `RunnerRun` record (§2.13), the
  project name, the CloudNativePG extension route (§2.8), and the
  decisions in §5.1. Everything else is perfectra1n's text.
- **Companion:** `ADR-0001-review.md`, the finding-by-finding review this ADR
  resolves. Finding IDs (B1–B5, S1–S9, E1–E4) refer to it. Not yet published
  alongside; requested.
- **Builds on:** [`home-operations/konflate`](https://github.com/home-operations/konflate)
  (forge, git, webhook and PR-filter code, copied), the `ai-review` composite
  action on the `feat/ai-review` branch of
  [`home-operations/.github`](https://github.com/home-operations/.github)
  (wrapping [`antongulin/robin`](https://github.com/antongulin/robin)).
- **Prior art:** [`kodustech/kodus-ai`](https://github.com/kodustech/kodus-ai)
  and [`kodustech/kodus-installer`](https://github.com/kodustech/kodus-installer),
  as examined in ADR-0001 §1.4; [`eleboucher/memini`](https://github.com/eleboucher/memini)
  for persistent embeddings, hybrid retrieval and store design in Go.
- **Implementation language:** Go, `CGO_ENABLED=0`, distroless static image.
- **Name:** kritik (German and Turkish for "critique"): module
  `github.com/home-operations/kritik`, environment prefix `KRITIK_`, comment
  marker `<!-- kritik:... -->`, in-repo file `.kritik.yaml`.

> Scope: this ADR covers product shape, tenancy, the reuse boundary with
> konflate, storage, process roles, configuration, the forge integration
> surface, git handling, retrieval, and how review quality is measured.
> Package layout, prompt design, and the embedding model default belong
> to follow-up ADRs. Chunking is decided here (§2.9), because retrieval
> (§2.10) and the schema (§2.13) depend on it.

This document is the canonical design for the service. ADR-0001 is
preserved unchanged as historical input. Most of its product decisions
stand and are carried forward here. Where this ADR differs, the change is
listed below with the finding that motivated it.

| Topic                                                              | ADR-0001                                                  | This ADR                                                                                                        |
| ------------------------------------------------------------------ | --------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| Product shape, operator-declared tenants, no public service        | §1.3                                                      | **Kept** (§1.3)                                                                                                 |
| Credentials as `env`/`file` references, nothing secret in Postgres | §2.4                                                      | **Kept**; file references are re-read so rotation works (§2.4, S6)                                              |
| konflate code copied, not shared                                   | §2.5                                                      | **Kept**; the git copy gains forge-sourced merge-base (§2.5)                                                    |
| Merge-base                                                         | "depth-1 fetch of head and base, merge-base"              | **Taken from the forge API**, then head and merge-base fetched at depth 1 (§2.9, B1)                            |
| Embedding model                                                    | Per-tenant `models.indexing`                              | **Deployment-wide**, recorded per index generation; a change is a reindex (§2.8, §2.12, B2)                     |
| Review job uniqueness                                              | River unique on tenant, repo, PR                          | **Unique on tenant, repo, PR and head SHA**; stale jobs discarded at start and before write-back (§2.8, B3)     |
| Model interface                                                    | One `Model` with `Complete` and `Embed`                   | **`Completer` and `Embedder`**, validated per role (§2.12, B4)                                                  |
| Row-level security                                                 | "a role the policies bypass", one database URL            | **Owner role and non-owner application role, two DSNs**, transaction-local tenant, startup assertion (§2.4, B5) |
| Per-tenant model concurrency                                       | Session advisory-lock slots                               | **Lease rows** claimed with `SKIP LOCKED`, heartbeat and expiry (§2.8, S1)                                      |
| Context retrieval                                                  | Embedding similarity                                      | **PR-head overlay, exact definitions, callers by `git grep`, then vector**, fused by budget (§2.10, S2, S3)     |
| Vector index                                                       | "HNSW partitioned by tenant and repository"               | **One `halfvec` table, HNSW, filtered by repository and index generation, iterative scans on** (§2.8, S4)       |
| Config reload                                                      | Every replica reloads and upserts                         | **Single-writer loader** under a leader lock; followers report drift (§2.6, S5)                                 |
| @-mention follow-ups                                               | Write access or higher                                    | **Plus a bot-loop guard** and per-thread rate limit (§2.7, S7)                                                  |
| Bot-authored PRs reviewed once                                     | Skip if diff unchanged against last merge-base            | **Skip if the diff's `git patch-id` is unchanged** (§2.7)                                                       |
| Quality bar                                                        | "Robin's quality or better"                               | **An evaluation harness and a measured gate** (§2.14, S8)                                                       |
| Retention                                                          | Rows disabled, never deleted                              | **Kept for history; vectors expire after a grace period** (§2.15, S9)                                           |
| Tree-sitter via `gotreesitter`, all grammars embedded              | §2.9                                                      | **Kept**; parity study moved to Appendix A, size to be measured (E1, E2)                                        |
| Section 1.5, "Origin of this document"                             | Present                                                   | **Dropped** (E3)                                                                                                |
| Runner pods, plain Jobs, `RunnerRun`                               | §2.3, §2.9, §2.12 (after 16:39 UTC)                       | **Kept**; retrieval stages split across runner and worker (§2.10)                                               |
| `vector` extension                                                 | Created by the CNPG `Database` resource (after 17:27 UTC) | **Kept**; never by a migration (§2.8)                                                                           |
| SQLite rejection rationale                                         | No CGO-free vector path                                   | **Corrected**; the rejection stands on test-surface grounds (§6, E4)                                            |
| v2 dashboard                                                       | §2.14                                                     | **Kept**, with the application role applying to `web` (§2.17)                                                   |
| AGPL-3.0, no CLA, no dual licensing                                | §2.13                                                     | **Kept** (§2.16)                                                                                                |

---

## 1. Context

### 1.1 What the org has today

- **A GitHub-only, CI-shaped review.** `actions/ai-review` runs Robin inside
  a workflow: it reads the PR diff through the GitHub API, sends it to an
  OpenAI-compatible endpoint, and posts a summary comment plus inline
  comments. It has taught two lessons the service must keep. A
  `REQUEST_CHANGES` review from a bot blocks auto-merge and evicts
  merge-queue entries, so findings are posted as a plain comment. And
  bot-authored PRs are reviewed once on open, because every Renovate rebase
  is a `synchronize` event on an unchanged diff.
- **No repository context.** Robin sees the diff and nothing else.
- **No GitLab or Forgejo path.** A GitHub Actions workflow cannot run for
  the self-hosted forges the org's own tools (konflate, flate) support.
- **A mature forge layer next door.** konflate lists, clones, renders and
  writes back to PRs on GitHub, GitLab and Forgejo, cloud or self-hosted,
  from one Go binary.

### 1.2 Why a service and not a bigger action

A repository-aware review needs an index that outlives one CI run, a
sticky comment identity, review history, and a follow-up channel. A
workflow job starts cold every time and cannot run on GitLab or Forgejo.

### 1.3 Deployment shape

**An operator runs one deployment for one or many forge accounts they
own.** An account is a GitHub organisation or personal account, a GitLab
group, or a Forgejo organisation. home-operations runs it for its own
accounts. Anyone else runs their own deployment with their own bots.
There is no public service and no GitHub App run on others' behalf.

- Tenants are **declared by the operator** in a configuration file. An
  installation of a shared public App from an undeclared account is logged
  and ignored. A tenant with its own private App avoids that exposure
  entirely.
- A homelab with one account is the same deployment with one tenant.
  Tenancy is always on, and there is no single-tenant code path.
- Every tenant is operator-trusted. Guards against untrusted input (forks,
  egress) remain as configurable defaults.

**Kubernetes is the runtime.** Every index and review job runs as a
Kubernetes Job in its own pod (§2.3), so the service requires a cluster and
RBAC to create Jobs. There is no compose or VM install. An in-process
executor exists for development and tests only.

### 1.4 What prior art established

From Kodus, as ADR-0001 §1.4 records: no Redis anywhere, with ownership,
idempotency and locking all in Postgres; per-tenant model concurrency as N
slots; no persistent git mirror; webhook ingest as a separate
verify-and-enqueue process; and an internal finding that building
repository context inside the review sandbox times out on large
repositories, so the index must be built ahead of time and updated
incrementally.

From memini, a Go service with the same storage stack, running in
production:

- **Persistent vectors make the embedding model part of the schema.**
  memini records the model that produced its vectors, refuses to start
  when the configured model differs, and treats re-embedding as an
  explicit, opt-in operation. The dimension is fixed when the table is
  created.
- **Similarity alone is not enough for retrieval.** memini runs a
  full-text index beside its vector index and fuses the two, and still
  needs score floors and reserved result slots. For code, where the most
  useful context is the definition or caller of a specific identifier,
  exact lookup matters even more.
- **A shared conformance suite does not remove the cost of a second
  backend.** memini's SQLite backend runs in every `go test`, while its
  Postgres backend runs only behind an `integration` build tag.

### 1.5 What this ADR resolves

ADR-0001's product design is sound, and most of it is carried forward.
Five of its mechanisms would not work as written: the git fetch plan, the
embedding schema, job uniqueness, the model interface, and the database
roles behind row-level security. Several more are underspecified. The
companion review documents each one with evidence. This ADR is the design
with those resolved.

---

## 2. Decision

### 2.1 Goals

- Whole-repo-aware review: the model sees the code the change touches,
  its definitions and its callers, not only the diff.
- One sticky summary comment per PR, edited in place on every push, plus
  inline findings where the forge supports line anchoring.
- Conversational follow-ups in the PR thread.
- GitHub, GitLab and Forgejo from day one, cloud and self-hosted.
- Multi-tenant from day one: tenant-scoped data, credentials, settings,
  concurrency limits and usage accounting, and a bot per tenant if wanted.
- Declarative: everything the service manages is declared in one file the
  operator keeps in git.
- One image, one binary, role-selectable at start. The only external
  dependency is Postgres with the `pgvector` extension.
- Bring-your-own review model per tenant (OpenAI-compatible endpoint or
  Anthropic direct), with an operator-provided default.
- **Measured quality:** review quality is scored on a fixed corpus, and
  retrieval and prompt changes are accepted or rejected on that score.

### 2.2 Non-goals

- **v1:** a dashboard or any authenticated HTTP surface (§2.17).
- A public hosted service, sign-up, billing or licensing.
- IDE or CLI surfaces.
- Replacing konflate (§2.11 describes the one integration point).
- **Per-tenant embedding models.** The embedding model is a property of
  the deployment's index, not a tenant preference (§2.8).

### 2.3 High-level architecture

```text
config file (tenants, installations, repositories, review models)
   -> loader (leader only): validate whole file, compile filters,
      resolve secret refs, upsert file-managed rows; reload on change

forge webhook  ->  ingest role: look up secret by /hooks/{installation},
                   verify signature, record PR head, enqueue. Never does work.

job queue (River, Postgres)
   -> index job      onboarding, default-branch push, or reindex
        -> worker creates a Kubernetes Job (same image, --role runner)
             pod: fetch default-branch tip (depth 1) -> parse -> chunk
                  -> write chunks (no vectors) under its job id
        -> worker: embed (deployment embedder) -> index generation
   -> review job     PR opened / pushed / @-mention
        -> worker: discard if head is no longer the PR head
        -> worker: tenant model lease (defer if none free)
        -> worker: merge-base from forge API; create a Kubernetes Job
             pod: fetch head + merge-base (depth 1) -> diff -> patch-id
                  -> context stages 1-3: overlay, definitions, callers
                  -> context pack written under its job id
        -> worker: skip unchanged bot PRs by patch-id
        -> worker: context stage 4 (embed hunks, similar chunks)
        -> worker: model call, fallback model on error
        -> worker: discard if head moved; write-back under per-PR lock:
           sticky comment, inline findings, check status

health and metrics on a separate port, as konflate does
```

One image with a `--role` flag: `all`, `ingest`, `worker`, `runner` in v1,
`web` added in v2. A homelab runs `all` with one replica; the runner pods
it spawns are the same image. A larger deployment runs `ingest` and
`worker` as their own Deployments with their own replica counts.

**Trust boundary.** The runner pod does everything that touches repository
contents: clone, parse, chunk, diff, exact lookups and `git grep`. It
holds a short-lived git read token for one repository and a database role
scoped to writing its own job's rows, and nothing else: no model key, no
forge write credential. The worker keeps the model calls, the leases and
the write-back. A fork's submodules, hooks and build scripts run in a
container that owns no secrets, with its own NetworkPolicy and resource
limits, and one huge repository cannot take down the worker serving other
tenants.

The v1 HTTP surface is `/hooks/{installation}` on the ingest role and
`/healthz`, `/readyz`, `/metrics` on the management port. Writes to forges
come only from the worker, using credentials held by that process, never
from a request or a runner pod. This keeps konflate's security property
and narrows it.

`/metrics` carries one series family per thing the service does: webhook
deliveries by outcome, reviews by terminal status with a duration
histogram, findings by severity, context chunks by stage, index runs by
mode and status, runner Jobs by kind and outcome with a duration
histogram, lease waits, and model calls with tokens and provider-reported
cost by tenant, model and role. Labels are bounded by configuration or by
fixed vocabularies; nothing per pull request or per commit is a label.
The README lists the series.

### 2.4 Tenancy, credentials and isolation

```text
Tenant  1..n  Installation (with its Credential)  1..n  Repository  1..n  PullRequest  1..n  Review
```

- **Tenant** is a forge account and the unit of isolation, settings,
  credentials, limits and usage accounting.
- **Installation** is one bot on one account: a GitHub App installation,
  or a GitLab or Forgejo bot with a group or org token. Each installation
  owns its credential and its webhook secret, and has its own hook URL,
  `/hooks/{installation-name}`, so ingest finds the right secret before it
  trusts a payload.
- **A GitHub App is a credential, not instance configuration.** Each
  tenant can bring its own private App. An instance-level shared App,
  configured by environment variables, is an optional default. Mixed
  deployments are fine.
- **No secret material is stored.** Every credential in the file is a
  reference to a file or an environment variable. Postgres holds
  references and metadata only.
- **Runner pods hold the minimum.** A runner receives one short-lived git
  token for its repository as an environment variable on the Job, and a
  DSN for a runner database role that can write chunk and context-pack
  rows for its own job id and read nothing else. The worker's use of the
  Kubernetes API is limited to creating, watching and deleting Jobs in its
  own namespace.
- **File references are re-read, not cached forever.** A file-referenced
  secret is read on use, through a cache with a one-minute TTL, and the
  cache entry is dropped immediately when the forge or model rejects the
  credential (HTTP 401 or 403). A rotated Kubernetes Secret takes effect
  within a minute without a config change or restart. Environment
  references are fixed for the life of the process.

**Isolation is enforced twice.**

1. **The `Store` scopes every query by tenant.** There is no unscoped
   query API on the paths that serve requests and jobs.
2. **Postgres row-level security backs it up.** Every tenant-scoped table
   has a policy keyed on a transaction-local setting. A query that forgets
   the tenant filter returns nothing, not another tenant's rows.

The row-level security design, precisely:

- **Two database roles, two DSNs.** An **owner role** owns the schema. It
  runs migrations and the loader, which writes across tenants. Because
  table owners are not subject to row security, the owner role bypasses
  the policies without needing `BYPASSRLS`. An **application role** owns
  nothing, is not a superuser, does not have `BYPASSRLS`, and is granted
  `SELECT, INSERT, UPDATE, DELETE` on the tables it uses. Every request
  and job runs as the application role. `FORCE ROW LEVEL SECURITY` is
  deliberately not used, because it would subject the owner role, and
  therefore the loader, to the policies.
- **The tenant is set per transaction.** The `Store` opens a transaction
  for each request or job step and runs
  `SELECT set_config('app.tenant_id', $1, true)`. The third argument makes
  the setting transaction-local, so it cannot survive on a pooled
  connection after commit or rollback.
- **An unset tenant matches nothing.** After a transaction-local setting
  ends, a custom setting reads back as the empty string, not NULL, and
  `''::uuid` raises an error instead of matching nothing. Policies
  therefore normalise it:

  ```sql
  ALTER TABLE chunks ENABLE ROW LEVEL SECURITY;
  CREATE POLICY tenant_isolation ON chunks
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
  ```

- **The service refuses to run with isolation silently off.** At startup,
  every role connecting with `DATABASE_URL` checks that its role is not a
  superuser, does not have `BYPASSRLS`, and owns no table in the schema.
  If any check fails, it exits with an error naming the check. A single
  misconfigured DSN would otherwise disable row-level security without any
  visible symptom.
- **River's tables are outside row-level security.** Job arguments carry
  the tenant ID, and only the service inserts jobs. A worker sets the
  tenant context from its job arguments before touching tenant-scoped
  tables.
- **Three roles in total.** Owner (migrations, loader, `chunks` DDL),
  application (every request and job in ingest, worker and, in v2, web),
  runner (job-scoped writes from pods). The runner role's policies key on
  the job id passed in its transaction setting, the same way the
  application role's key on the tenant.

**Usage accounting:** one row per model call with tenant, repository,
review, role (`review`, `fallback`, `embedding`), model, input and output
tokens, exported as metrics per tenant and repository. Embedding usage is
recorded against the tenant whose repository was indexed. Optional
per-tenant caps on reviews per day and tokens per month are enforced in
the review worker before the model call. They are unset by default.

### 2.5 Reuse boundary with konflate

konflate's forge code is **copied into this repository, not shared**. The
two services diverge immediately. konflate is one repository per process
with process-wide credentials. This service is many repositories per
tenant with per-installation credentials, and a shared module would have
to carry both shapes from its first release. If the copies converge
later, extraction is a follow-up.

| konflate package                                                  | Copy                                                            | Change on arrival                                                                                                                                                                                     |
| ----------------------------------------------------------------- | --------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/gitclone`                                               | as-is                                                           | The persistent bare mirror becomes an optional cache. Add an ephemeral mode: temp dir, depth-1 fetch of explicit commits (§2.9), cleanup. Merge-base is an input from the forge, not computed locally |
| `internal/prfilter`                                               | as-is                                                           | None; supply this service's own `pr` variable map                                                                                                                                                     |
| `internal/webhook`                                                | with the forge-kind enum                                        | Secret lookup by installation name from the hook path. Add comment-created events (§2.7)                                                                                                              |
| `ForgeURI`, `ParseForgeURI` from `internal/config/forge.go`       | as-is                                                           | None                                                                                                                                                                                                  |
| `internal/provider` (`provider.go`, `writer.go`, per-forge files) | with the GitHub App JWT and installation-token transport intact | Constructors take an options struct per installation. Add `MergeBase(ctx, pr)`, inline review comments (§2.7), and this service's own `PR` type                                                       |
| `internal/config` pattern                                         | as a pattern                                                    | One `Config` struct via `caarlos0/env` for process configuration, with `,unset` scrubbing of secrets                                                                                                  |

Copied as patterns, not code: the per-PR coalescing queue (replaced by
head-SHA-keyed jobs plus staleness checks, §2.8), the keyed write mutex
(replaced by a per-PR transaction advisory lock during write-back), and
retry with backoff.

The GitHub App JWT and installation-token transport is the piece most
worth copying verbatim and keeping in sync with konflate by hand when
either side fixes a bug in it.

### 2.6 Configuration

Two sources, one direction.

**Environment variables** configure the process, konflate-style, all
prefixed `KRITIK_`. The role is the `--role` flag rather than a variable.

| Variable                                                                                   | Purpose                                                                                                                                                            |
| ------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `KRITIK_DATABASE_URL`                                                                      | Application role DSN. Ingest and worker use it; runner pods receive the runner role's DSN instead                                                                  |
| `KRITIK_DATABASE_OWNER_URL`                                                                | Owner role DSN. Needed where migrations, the loader and the `chunks` DDL run: `all` and `worker` (leader-eligible). The chart's optional migration Job uses it too |
| `KRITIK_CONFIG_FILE`                                                                       | Path to the configuration file                                                                                                                                     |
| `KRITIK_CONFIG_RELOAD_INTERVAL`                                                            | Poll interval for the file, default 10 s                                                                                                                           |
| `KRITIK_ADDR`, `KRITIK_METRICS_ADDR`                                                       | Hook listener, and health and metrics listener                                                                                                                     |
| `KRITIK_RUNNER_IMAGE`, `KRITIK_RUNNER_SERVICE_ACCOUNT`, `KRITIK_RUNNER_TTL_SECONDS`        | Runner Job defaults; the chart sets them from its own values                                                                                                       |
| `KRITIK_EMBED_BASE_URL`, `KRITIK_EMBED_API_KEY`, `KRITIK_EMBED_MODEL`, `KRITIK_EMBED_DIMS` | Deployment-wide embedder, OpenAI-compatible (§2.8)                                                                                                                 |
| `KRITIK_EMBED_MAX_BATCH`, `KRITIK_EMBED_MAX_BATCH_CHARS`, `KRITIK_EMBED_MAX_ITEM_CHARS`    | Embedding request limits; OpenAI-compatible servers differ widely in what they accept                                                                              |
| `KRITIK_REINDEX_ON_MODEL_CHANGE`                                                           | `false` by default. See §2.8                                                                                                                                       |
| `KRITIK_GITHUB_APP_*`                                                                      | Optional instance-level shared App                                                                                                                                 |
| `KRITIK_RESTRICT_EGRESS`                                                                   | Egress guard inside the runner, default follows the fork setting (§2.7)                                                                                            |

Secrets are `unset` after load. The embedding configuration is deliberately
in the environment, not the file. Changing it reindexes every repository,
so it should take a deploy, not a config reload.

**The configuration file** declares everything the service manages:
review model providers, defaults, tenants, installations, repositories. It
is mounted from a ConfigMap and reloaded on change.

```yaml
# Review model endpoints. Keys are references, never values.
providers:
  openrouter:
    type: openrouter # server-side fallback, cost reporting
    apiKey: { env: OPENROUTER_API_KEY }
  local:
    type: openai # any OpenAI-compatible endpoint
    baseUrl: http://litellm.ai.svc:4000/v1
    apiKey: { file: /var/run/secrets/litellm/api-key }

# Applied to every tenant unless overridden. Model names illustrative.
defaults:
  models:
    review: openrouter/openai/gpt-6-sol
    fallback: openrouter/anthropic/claude-sonnet-5
  filter: "!pr.draft"
  forks: false
  limits:
    concurrency: 2 # model leases per tenant and model

retention:
  disabledIndexGrace: 720h # vectors of disabled repositories

tenants:
  - slug: home-operations
    installations:
      - name: sticky-gecko # hook URL: /hooks/sticky-gecko
        forge: github
        account: home-operations
        app:
          clientId: Iv1.xxxxxxxx
          privateKey: { file: /var/run/secrets/sticky-gecko/private-key.pem }
          webhookSecret: { file: /var/run/secrets/sticky-gecko/webhook-secret }
    limits:
      reviewsPerDay: 200
    repositories: # overrides only; everything the
      - name: home-operations/flate # installation grants is watched
        konflate: https://konflate.example.org
      - name: home-operations/kopiur
        filter: '!pr.draft && !pr.labels.exists(l, l.name == "skip-review")'
      - name: home-operations/charts-mirror
        enabled: false

  - slug: onedr0p
    installations:
      - name: bot-ross
        forge: github
        account: onedr0p
        app:
          clientId: Iv1.yyyyyyyy
          privateKey: { file: /var/run/secrets/bot-ross/private-key.pem }
          webhookSecret: { file: /var/run/secrets/bot-ross/webhook-secret }
      - name: onedr0p-forgejo
        forge: forgejo
        host: git.example.org
        account: onedr0p
        token: { file: /var/run/secrets/forgejo-bot/token }
        webhookSecret: { file: /var/run/secrets/forgejo-bot/webhook-secret }
    models:
      review: local/claude-opus-5
    forks: true
```

Rules:

- **Secret references are `env` or `file` only.** No Kubernetes API, no
  vault client.
- **Repositories are opt-out.** Everything an installation grants access
  to is watched. The `repositories` list carries overrides and
  `enabled: false`.
- **Settings resolve in one direction:** `defaults`, then tenant, then
  repository, then the in-repo file, which can only narrow. It can tighten
  the filter or disable review. It cannot enable forks, raise a limit or
  change a model.
- **The file is validated whole.** Every CEL filter is compiled and
  smoke-tested against a sample PR. Every reference is resolved. Every
  installation name is unique. Every model reference names a provider
  with the capability its role needs (§2.12). Unknown keys are errors. A
  bad file is rejected with the error logged, and the last good state
  stays live.
- **The file is not versioned in-band.** There is no `apiVersion` field.
  The schema is documented, validated strictly, and changed with release
  notes.
- **Every role except runner loads the file; one loader writes.** Ingest
  needs the file in memory to resolve webhook secrets, which are
  references and never in Postgres. Worker-capable replicas (`all`,
  `worker`) compete for a session-level advisory lock on a dedicated
  connection. Every pool asks Postgres for TCP keepalives on its sessions
  (idle 30 s, interval 10 s, three probes), so the session of a replica
  that died without closing it, and the lock with it, is dropped within
  about a minute rather than the kernel's default of hours; the owner
  pool also carries a ten-minute statement timeout. The holder is the
  leader and is the only replica that applies the file to Postgres. It runs pending migrations first, ensures
  the `chunks` table exists at the configured embedding dimension, then
  upserts tenants, installations and repositories as rows with
  `managed_by = file`, and records the applied content hash in a one-row
  `config_state` table. Other replicas never write configuration. They
  compare their mounted file's hash with `config_state` and expose a
  mismatch as a metric and a log line, never on `/readyz`: a stale
  ConfigMap on one node must not take an ingest replica out of the Service
  and drop webhooks. Kubernetes updates mounted ConfigMaps in place, so the
  leader converges on the newest file. The chart must not mount the file
  with `subPath`, because `subPath` mounts are never updated.
- **Rows absent from the file are disabled, not deleted.** History and
  review records survive a mistake. Vectors of disabled repositories
  expire after `retention.disabledIndexGrace` (§2.15).
- **Postgres is the source of truth for the workers**, even though the
  file is the only writer in v1. This is what makes the v2 dashboard
  additive.

The in-repo file, `.kritik.yaml` at the repository root, uses the same
vocabulary restricted to what may narrow, plus an `ignore` list of path
globs that chunking and the caller search (§2.10) skip. Vendored and
generated trees otherwise dominate both:

```yaml
filter: '!pr.labels.exists(l, l.name == "wip")'
enabled: true
ignore:
  - vendor/**
  - "**/*.pb.go"
  - "**/testdata/**"
```

`ignore` may also be set per repository in the operator's file; the two
lists are unioned. The built-in default covers `vendor/`, `node_modules/`
and lockfiles.

### 2.7 Forge integration surface

|                  | GitHub                                                            | GitLab                                      | Forgejo                                                  |
| ---------------- | ----------------------------------------------------------------- | ------------------------------------------- | -------------------------------------------------------- |
| Install          | GitHub App, the tenant's own or the instance's shared one         | Bot user plus group or project access token | Bot account plus token                                   |
| API and git auth | Installation token, minted and refreshed per installation         | Token                                       | Token                                                    |
| Merge-base       | Compare `base...head`: `merge_base_commit.sha`                    | Merge request `diff_refs.base_sha`          | Pull request `merge_base`                                |
| PR head ref      | `refs/pull/{n}/head`                                              | `refs/merge-requests/{n}/head`              | `refs/pull/{n}/head`                                     |
| Bot identity     | `<app-slug>[bot]`, resolved from the App                          | Bot user, resolved from the token           | Bot user, resolved from the token                        |
| Sticky comment   | Matched by **author id plus hidden marker**                       | Note matched by author plus marker          | Comment matched by poster plus marker                    |
| Inline findings  | Pull request review comments                                      | Discussions with a position                 | Pull review with state `COMMENT` and positioned comments |
| Status           | Check Run (`success` or `neutral`), falling back to commit status | Commit status                               | Commit status                                            |
| Webhook          | `X-Hub-Signature-256` HMAC                                        | `X-Gitlab-Token`                            | `X-Gitea-Signature` HMAC                                 |
| Self-hosted      | GHES via `github://host/owner/repo`                               | `gitlab://host/group/repo`                  | `forgejo://host/org/repo`                                |

**Review state is `COMMENT` on every forge**, never `REQUEST_CHANGES`, and
the check conclusion is never `failure`. The service never blocks a
merge.

**Follow-ups.** Comment-created events (`issue_comment` and
`pull_request_review_comment` on GitHub and Forgejo, `note` on GitLab) are
parsed into a `Comment` event with PR number, author, comment ID, body and,
for a reply on an inline finding, the finding it belongs to. A follow-up on
an inline finding is scoped to that finding. A
follow-up requires all of the following:

- The comment @-mentions the installation's resolved bot identity, never a
  hard-coded name.
- The author has write access or higher.
- The author is not this service's bot identity for any installation, and
  is not an account the forge marks as a bot (`type: Bot` on GitHub,
  `bot: true` on GitLab users, and the bot flag on Forgejo users).
- The thread has had fewer than five follow-ups in the past hour. Further
  mentions get a single reply saying the limit was reached.

**Bot-authored PRs are reviewed once per distinct change.** On a
`synchronize` for a bot-authored PR, the worker computes the stable
`git patch-id` of the merge-base-to-head diff. If it equals the patch-id
of the last completed review, the job completes without a model call.
A Renovate rebase moves the merge-base but leaves the patch-id unchanged,
so it is skipped. A Renovate update that changes the diff gets a new
review. As built, the check runs twice: first on the forge's own diff of
the pull request, before a lease or a runner is spent (merging one
Renovate PR rebases all its siblings, and each would otherwise cost a
runner pod to be skipped), comparing only with the forge patch-ids of
earlier reviews, since the forge's diff is not the runner's byte for byte;
then on the runner's diff, for whatever the forge could not tell. A manual
re-run is never skipped.

**Egress guard.** konflate's `RESTRICT_EGRESS` guard (private, loopback,
link-local and metadata ranges blocked, `https` and `ssh` only) is
carried over. It defaults on whenever forks are enabled for any tenant.

### 2.8 Storage, queue and coordination

Postgres only, with `pgvector` as a hard requirement. No SQLite mode, no
Redis, no broker.

`pgvector` is required because the alternative, an in-process scan of a
repository's whole vector set on every review, moves tens of megabytes
per review at monorepo scale. It is the only extension the service uses.
kritik never creates it: the owner role is deliberately not a superuser.
On CloudNativePG the declarative `Database` resource does it
(`spec.extensions: [{name: vector, ensure: present}]`), which the operator
applies as superuser; this is verified working in the development
deployment. Elsewhere
the operator pre-creates it. Every role checks `pg_extension` at startup
and refuses to start without it. CloudNativePG's `standard` images bundle
pgvector and its `minimal` images do not. The chart's `values.yaml` and
README say so next to the DSN values.

| Concern                       | Mechanism                                                                                                                     |
| ----------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Relational state              | Postgres, tenant-scoped with row-level security (§2.4)                                                                        |
| Vectors                       | One `chunks` table, `halfvec(KRITIK_EMBED_DIMS)`, HNSW with `halfvec_cosine_ops`, filtered by repository and index generation |
| Job queue                     | River                                                                                                                         |
| Webhook idempotency           | River job unique on the forge delivery ID                                                                                     |
| Review job uniqueness         | River job unique on tenant, repository, PR and **head SHA**                                                                   |
| Coalescing                    | Staleness check against `pull_requests.head_sha` at job start and before write-back                                           |
| Per-tenant model concurrency  | Lease rows in `model_leases`, claimed with `FOR UPDATE SKIP LOCKED`                                                           |
| Per-PR write serialisation    | `pg_advisory_xact_lock` on the PR, held only for the write-back transaction                                                   |
| Config loading and migrations | Leader lock plus `config_state` (§2.6)                                                                                        |
| Runner execution              | Kubernetes Jobs created by the worker; state in `runner_runs` (§2.9, §2.13)                                                   |

**Review jobs and coalescing.** River's default unique states include
`running` and `completed`, and River requires `running` whenever the
state list is customised. A unique key without the head SHA would drop a
push that arrives during or after a review. Instead:

```go
type ReviewArgs struct {
    TenantID     uuid.UUID `json:"tenant_id"     river:"unique"`
    RepositoryID uuid.UUID `json:"repository_id" river:"unique"`
    Number       int       `json:"number"        river:"unique"`
    HeadSHA      string    `json:"head_sha"      river:"unique"`
    Trigger      string    `json:"trigger"` // opened, synchronize, poll
}

func (ReviewArgs) Kind() string { return "review" }

func (ReviewArgs) InsertOpts() river.InsertOpts {
    return river.InsertOpts{Queue: "review", UniqueOpts: river.UniqueOpts{ByArgs: true}}
}
```

- Ingest updates `pull_requests.head_sha` and inserts the job **in the
  same transaction**. Webhook redeliveries and poll rediscoveries of the
  same head collapse into one job. Every new head gets its own job.
- The worker reads `pull_requests.head_sha` when it starts. If the job's
  head is not the current head, the job completes as `superseded` without
  a model call. It checks again inside the write-back transaction, under
  the per-PR lock, and discards the result if the head moved while the
  model ran. The newest head's own job produces the review.
- Follow-up jobs are unique on the comment ID. Index jobs are unique on
  repository and target commit.

This gives konflate's "latest wins" behaviour without losing a push.

**Model leases.** Session advisory locks would hold a pooled connection
for the whole model call, which can take minutes. Leases hold nothing:

```sql
CREATE TABLE model_leases (
  tenant_id  uuid    NOT NULL,
  model_key  text    NOT NULL,     -- provider/model
  slot       int     NOT NULL,
  job_id     bigint,
  expires_at timestamptz,
  PRIMARY KEY (tenant_id, model_key, slot)
);

-- claim, in the job's tenant transaction
UPDATE model_leases SET job_id = $1, expires_at = now() + interval '2 minutes'
WHERE (tenant_id, model_key, slot) = (
  SELECT tenant_id, model_key, slot FROM model_leases
  WHERE tenant_id = $2 AND model_key = $3
    AND (job_id IS NULL OR expires_at < now())
  ORDER BY slot LIMIT 1
  FOR UPDATE SKIP LOCKED)
RETURNING slot;
```

The loader keeps `limits.concurrency` slot rows per tenant and model. The
holder renews `expires_at` every 30 seconds while the call runs, and
clears `job_id` when it finishes. A crashed worker's lease expires on its
own. A job that gets no slot snoozes with capped exponential backoff (up
to five minutes). A fallback call releases the primary model's lease
first, then takes one on the fallback model's key, so a job never holds
two slots.

**Embedding model and vector schema.** The embedding model is
deployment-wide:

- `KRITIK_EMBED_DIMS` fixes the `halfvec` dimension of the `chunks` table.
  The table is not part of the migrations, which stay free of
  environment-dependent DDL; the leader creates it, with its policies and
  grants, at startup once the dimension is known, and the DDL is
  idempotent. `halfvec` supports HNSW indexes up to 4,000 dimensions, and
  `vector` only up to 2,000, which would rule out several current models.
- Every `IndexRun` records the embedding model and dimension that produced
  its chunks. The deployment's current model and dimension are also
  recorded in store metadata.
- At startup, if `KRITIK_EMBED_MODEL` differs from the recorded model but
  the dimension is the same, the worker refuses to start unless
  `KRITIK_REINDEX_ON_MODEL_CHANGE=true`. With it set, the worker enqueues a
  rate-limited reindex of every repository into new index generations
  (§2.9). Until a repository's new generation is complete, its reviews run
  without vector retrieval rather than with mismatched vectors. Exact and
  caller context (§2.10) is unaffected.
- A dimension change cannot be done in place. `kritik reindex --recreate`
  drops and recreates `chunks` at the new dimension and reindexes
  everything. It is an explicit operator action.

**Vector index shape.** One table, not partitions. Queries filter on
`repository_id` and `index_run_id`, and run with
`SET LOCAL hnsw.iterative_scan = relaxed_order` so that a small repository
in a large index still returns its full result count (pgvector 0.8+). A
B-tree index on `(repository_id, index_run_id, path)` serves incremental
updates and path lookups. Per-repository partitions were rejected,
because they put DDL on the onboarding path and multiply indexes by
repository count. If filtered recall on the evaluation corpus (§2.14)
falls short, the first alternative is VectorChord behind the same `Store`
interface, as memini uses (§6).

### 2.9 Git and indexing

**Git is a pod per job.** The worker creates a Kubernetes Job from the
same image with `--role runner`, `ttlSecondsAfterFinished` set, a resource
limit sized per tenant (the `runner` block in the file), and an
`activeDeadlineSeconds` that bounds a runaway clone or parse. No worker
owns a repository, and any replica can take any job. A per-node image
cache is the only warm state.

**Plain Jobs, no CRD.** Each Job carries labels for tenant, repository, PR
number and kind (`index` or `review`) and annotations for the River job id
and head SHA, so `kubectl get jobs -l tenant=<slug>`, per-job logs and
events, and a Grafana panel on those labels come for free. A `ReviewRun`
custom resource was considered and rejected (§6).

**Every Job is recorded.** The worker writes a `runner_runs` row per Job
(§2.13): pod and node, phase timestamps, exit code and termination reason,
and the last 64 KB of stdout and stderr fetched once at completion, before
the TTL deletes the Job. The runner updates its own phase (cloning,
parsing, writing) as it goes. Nothing about a run is lost when the Job is
garbage-collected, and the v2 dashboard reads runs from Postgres, never
from the Kubernetes API.

**The git token is a Secret per run.** The worker writes the installation
token into a Secret named after the Job, references it from the Job's
environment, and makes the Job its owner once created, so the TTL that
removes the Job removes the Secret. The token never appears in the Job
spec, which anyone allowed to read Jobs could read. ADR-0003 §2.9 extends
this Secret into the run's job-scoped Secret (the job document and, in
agentic mode, the model key beside the token), and has the leader delete
one no Job owns by name, from the run's row, so the worker Role still has
neither `get` nor `list` on Secrets.

**A Job never outlives its queue job.** River bounds every job with a
timeout, one minute by default, and cancels its context past it. Each
worker sets its own from the tenant's runner deadline (the agent's timeout
in agentic mode) plus headroom for the lease wait, publish or embedding
pass, capped at three hours; a follow-up gets thirty minutes. Stuck jobs
are rescued only after that cap plus an hour. When the context ends while
a runner Job is still running, whether by timeout, supersession or a
worker shutting down, the executor deletes the Job and its pod before
returning, so no runner finishes work nobody will read; the run row keeps
what the Job reported up to that point.

**Review fetch, in the runner pod:**

1. The worker asks the forge for the PR's merge-base (§2.7) and passes the
   head and merge-base SHAs in the Job spec.
2. The runner does `git fetch --depth=1` of the head SHA and the merge-base
   SHA into its scratch dir. Two trees are enough to diff. `git diff`
   compares trees and needs no history.
3. It diffs merge-base to head, computes `git patch-id --stable`, and uses
   the head worktree for context stages 1 to 3 (§2.10).
4. It writes the diff, the patch-id and the context pack under its job id
   and exits; the pod's filesystem goes with it.

If the forge returns no merge-base, or refuses a fetch by SHA, the runner
fetches the PR head ref and the base branch, and deepens both
(`git fetch --deepen=50`, repeated, capped at 1,000 commits) until
`git merge-base` resolves. After the cap, it does one full fetch. Each
fallback is counted in a metric per installation. Whether each forge
accepts fetches by SHA without a ref is an open question to verify before
implementation (§5).

**Indexing:**

- **The index describes the default branch.** It is built at onboarding
  and kept current on default-branch pushes. Onboarding is the leader's
  job: after every configuration apply it enqueues an index job for each
  enabled repository without an active generation, with no commit named,
  and the worker asks the forge for the default branch tip. Index jobs
  are unique per repository and commit only while queued or running, so
  this is safe to repeat.
- **Index generations.** An `IndexRun` is a generation of a repository's
  index: commit SHA, embedding model, dimension and status. A repository
  has at most one active generation. A full build (onboarding, model
  change, unreachable previous commit) writes a new generation and makes
  it active atomically when complete. The previous generation's chunks
  are deleted afterwards.
- **Incremental updates change the active generation in place.** On a
  default-branch push, the runner fetches the previous and new tips at
  depth 1, diffs the two trees for changed paths, and stages the
  re-chunked paths under its job id (`index_staging`, with an
  `index_packs` row naming the mode and the changed paths). The worker
  embeds them and, in one transaction, deletes those paths' old chunks,
  inserts the new ones with their vectors, and advances the generation's
  commit SHA. If the previous tip cannot be fetched (a force push or a
  long gap), the runner itself falls back to a full build and says so in
  the pack, so the worker never retries the Job.
- **The vector table is created by the leader, not a migration.** A
  `halfvec` column's dimension is fixed at creation and the dimension is
  deployment configuration (`KRITIK_EMBED_DIMS`), so the leader creates
  `index_chunks` (HNSW, cosine) at that dimension on first start and
  records model and dimension in `index_schema`. A later start with a
  different model or dimension is refused unless
  `KRITIK_REINDEX_ON_MODEL_CHANGE` is set, which detaches every
  generation, drops the table, and lets onboarding rebuild each
  repository.
- **Indexing is rate-limited separately from review, per tenant**, so
  onboarding a large account does not starve reviews: its own River
  queue with `KRITIK_INDEX_WORKERS` workers, and embedding calls take a
  lease on `embed:<model>` under the tenant's concurrency limit. Files
  whose grammar has no declarations (configuration formats) are indexed
  as fixed windows of 60 lines overlapping by 10, so a GitOps repository
  is searchable too.

**Chunking** (carried from ADR-0001 §2.9, with metadata made explicit):

- **Chunks are AST nodes via tree-sitter.** Functions, methods, types and
  top-level declarations become chunks. Each records its path, byte
  range, language, **symbol name**, kind (function, method, type, other),
  signature and enclosing scope. The symbol column is what exact lookup
  (§2.10) reads.
- **Parsing is pure Go** through
  [`odvcencio/gotreesitter`](https://github.com/odvcencio/gotreesitter), so
  the build stays `CGO_ENABLED=0`. ADR-0001's parity check found it at
  parity with the C runtime on the org's repositories. The study is kept
  in Appendix A.
- **The parser is behind a `Parser` interface** with two implementations
  from day one: gotreesitter and a fixed-size chunker. A parse that hits a
  safety cap or fails outright falls back for the whole file. A parse
  that succeeds with error nodes falls back only for the affected
  top-level declarations. The official CGO bindings remain a possible
  third implementation.
- **All grammars are embedded.** Any repository an operator points the
  service at parses without a rebuild. A grammar whose tags query is
  empty (the configuration formats) yields no declarations and its files
  take the fixed-window path, which is the allowlist. Measured on
  2026-09-24 on a stripped static build: the chunker with all 206
  grammars links 28 MiB on its own, and the whole binary is 122 MiB, of
  which Fantasy's OpenRouter provider accounts for 43 MiB (it imports the
  Anthropic and Google providers, and with them the AWS SDK and genai)
  and client-go for 25 MiB. Accepted as is; build-tag subsetting
  (`grammar_subset` plus one tag per language) is available if that ever
  matters.

### 2.10 Context retrieval

The model receives the diff plus a context block assembled in priority
order, until a token budget (configurable per tenant, default 24,000
tokens) is spent. Each stage adds only what earlier stages did not
include. Tokens are counted with a per-provider characters-per-token
approximation, rounded conservatively; exact tokenizers differ per model
and the budget is a ceiling, not a target.

**Stages 1 to 3 run in the runner pod** and are written into the context
pack, because they need the checkout. **Stage 4 runs in the worker**,
because it needs the embedder key. Nothing else crosses the boundary.

1. **PR-head overlay.** For each path the PR modifies, the head version's
   changed declarations are parsed and chunked in memory, the same way
   the index would chunk them. They replace any index chunk for the same
   path. They are not written to the index. The index describes the
   default branch, so without this overlay every chunk from a changed path
   would show pre-change code.
2. **Definitions.** Identifiers declared or referenced on changed lines
   are extracted from the tree-sitter parse of the diff's two sides. Their
   definitions are found by the same head-tree word search as stage 3: a
   hit whose enclosing declaration carries that name is a definition.
   The active generation's `symbol` column could answer the same
   question without a tree walk; that shortcut is deferred, since the
   walk is bounded and always current. Identifiers are capped per review
   (default 40, most frequent first) and definitions per identifier
   (default two, different files first).
3. **Callers.** Each changed declaration's name is searched, word-exact,
   over the head tree the runner holds, skipping the `ignore` paths
   (§2.6) and every file whose grammar has no declarations. The search is
   in-process over go-git's tree rather than `git grep`, since the runner
   holds a bare fetch and no worktree; only files with a hit are parsed
   to map hits to their enclosing declaration. The walk is bounded (file
   count, bytes, per-file size) and reports when it stopped early.
   Callers are capped per symbol (default five), preferring hits in
   different files. Stages 2 and 3 are one walk.
4. **Similar code.** The diff hunks are embedded and the nearest chunks
   in the active generation are retrieved, with a similarity floor. This
   stage finds conventions and related code that share no identifiers.

Stages 2 and 3 are exact and deterministic. They also cover the part of
Kodus's call-graph context gain (F1 0.338 to 0.464, per ADR-0001) that
needs no graph. Every assembled context records which stage contributed
each chunk. The evaluation harness (§2.14) ablates stages using that
record. Full call-graph extraction remains a follow-up.

### 2.11 Review pipeline

1. **Onboard.** The operator declares the tenant and installation in the
   file. On GitHub, the App's installation webhook attaches the
   installation and enqueues an index job per selected repository. On
   GitLab and Forgejo, the operator adds the webhook by hand. The service
   logs the URL and expected header at load.
2. **Index** as in §2.9.
3. **PR event**, by webhook or by the periodic poll that backstops missed
   webhooks. The poll is a leader duty: every `KRITIK_POLL_INTERVAL`
   (default ten minutes) it lists, per installation and enabled
   repository, the open PRs updated since the last poll (persisted in
   `poll_state`, bounded by `KRITIK_POLL_LOOKBACK` on a first or long-idle
   poll) and hands each to the ingest dispatcher as a synthetic `poll`
   event, so the fork gate, filter and upsert are the webhook's. Because
   review jobs are unique on the head SHA, the poll can rediscover an
   already queued head without creating a duplicate. An installation's
   first poll hands over the PRs last updated before kritik knew the
   installation as `baseline` events instead: they are recorded, so the
   dashboard lists them and a review can be asked for there, but not
   reviewed. No webhook for them was missed, and on a large install
   reviewing them would start kritik with a review of every recently
   active PR at once.
4. **Filter gate.** The fork gate, then the resolved CEL expression.
   Filtered PRs enqueue nothing.
5. **Staleness check.** Discard the job if its head is not the PR's head.
6. **Tenant limits.** Take a model lease, and check the daily review and
   monthly token caps when they are set, under the lease so concurrent
   reviews cannot all pass a cap of one. Jobs that hit a cap are marked
   as such and counted in a metric.
7. **Review.** Merge-base from the forge, then a runner Job for fetch,
   diff, patch-id and context stages 1 to 3; the worker skips an unchanged
   bot PR by patch-id (§2.7), runs stage 4, assembles the prompt, calls the
   model, and the fallback model on error.
8. **Write-back.** In one transaction holding the per-PR advisory lock,
   the worker rechecks the head, edits the sticky comment, posts inline
   findings, and sets the check status, retrying with backoff. The
   transaction has a 60-second timeout. A review that finds nothing says
   so.
9. **Follow-up.** A qualifying mention (§2.7) re-enters the worker scoped
   to that thread, with the thread, the original findings and the review's
   recorded context in the prompt. The worker fetches the comment back
   from the forge rather than trusting the webhook body, checks the
   mention against the App's own slug and the author's permission
   through the forge, and posts one reply: on the conversation for an
   issue comment, under the root inline comment for a reply on a finding.
   Each mention leaves a `followups` row (answered, limited, ignored with
   the reason, or failed), unique per comment, and a `usage` row with
   role `followup`. The hourly limit is counted from those rows per pull
   request; the limit notice is posted once per hour.

**konflate integration (optional, per repository).** For a Flux
repository, the worker can fetch konflate's rendered `DiffResult` for the
same PR (blast radius, image changes, danger lint) and include it in the
prompt. It is the `konflate:` key on a repository in the file. This is a
stretch goal for v1. The service must review non-GitOps repositories with
no konflate present.

### 2.12 Model adapters

```go
type Completer interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

type Embedder interface {
    Embed(ctx context.Context, inputs []string) ([][]float32, error)
}
```

**v1 is OpenRouter only**, decided 2026-09-24 and amended by
[ADR-0003](0003-forgejo-agentic-review.md) §2.5, which adds direct
Anthropic and OpenAI providers and replaces Fantasy. OpenRouter is one
OpenAI-compatible endpoint that covers every call the service makes:
`/chat/completions` for structured findings, `/embeddings` for the
deployment-wide embedder, a per-request `models` list for server-side
fallback, and token counts plus dollar cost in every response. Self-hosted
and other vendors' models are reached through OpenRouter's catalogue. The
provider types are `openrouter` and `openai`; the second is any
OpenAI-compatible endpoint named by `baseUrl` (LiteLLM, vLLM, Ollama) and
exists because the same adapter serves it for free, without fallback or
cost reporting. A direct Anthropic adapter is deferred until a tenant
needs one.

Structured output is a forced tool call named after the schema, which is
how Fantasy asks OpenAI-compatible servers for objects and what works
across OpenRouter's catalogue; `response_format: json_schema` is not
used. Fantasy validates the tool arguments against the schema before the
worker sees them, and the worker then drops any finding it cannot anchor
to a line the diff shows.

**Libraries.** Completions go through
[`charm.land/fantasy`](https://github.com/charmbracelet/fantasy) and its
`openrouter` provider (Apache-2.0, weekly releases, built for Charm's
Crush). Its per-call provider options carry OpenRouter's routing
preferences and an `ExtraBody` map for the `models` fallback list, and its
response metadata returns OpenRouter's usage accounting with `cost`.
Embeddings go through `openai/openai-go`, the SDK Fantasy already builds
on, pointed at OpenRouter's base URL. Fantasy has no embedding interface
today; an outside pull request adding one has been open since January
without a maintainer decision. When it lands, embeddings move to Fantasy
and the second dependency disappears. Both stay behind the interfaces
above; a hand-written client and a multi-provider SDK were the
alternatives (§6).

- Tenant roles are `review` and `fallback`, each a `provider/model`
  reference, falling back to `defaults`. A fallback on the same provider
  is sent server-side as OpenRouter's `models` list, so the worker holds
  one lease on the primary model and no release-then-retake step exists.
  Fantasy does not expose which model OpenRouter actually answered with,
  so usage is recorded against the requested model with the upstream
  provider name alongside. A fallback on a different provider is a second
  call from the worker after the first fails, recorded with role
  `fallback`.
- The lease is `model_leases` rows per tenant and model reference, one
  per configured `concurrency` slot, created on demand and claimed with
  `FOR UPDATE SKIP LOCKED`; the holder renews every 30 s and a lease
  older than 2 min is free to take. A review checks for a free slot
  before any forge call or runner and, finding none, snoozes (5 s
  doubling to 5 min, jittered, not counted as an attempt), giving its
  worker back to the queue; an agentic review snoozes too when its lease,
  taken before its runner, is gone by the time it asks. A job that has
  already done its expensive work (a single-mode model call, an
  embedding pass, a follow-up) waits in process instead, 2 s doubling to
  30 s, jittered, since a snooze would do that work again.
- Caps (`reviewsPerDay`, `tokensPerMonth`) are checked before the model
  call from `reviews` and `usage`; an exhausted cap ends the review as
  `capped` with the reason in `error` and posts nothing.
- Write-back order is the sticky comment first (created once per PR,
  edited afterwards, found by the stored comment id or by author plus
  marker), then a `COMMENT` review with one inline comment per finding on
  the head commit, then the `kritik/review` commit status. Only the sticky
  comment can fail the review; the other two are logged and skipped, so a
  forge quirk never becomes a retry storm.
- The deployment embedder is built from the `KRITIK_EMBED_*` variables
  (§2.6), defaulting to OpenRouter's base URL. Batches are split to
  respect the `KRITIK_EMBED_MAX_*` limits.
- Every call records usage (§2.4), including OpenRouter's reported cost.

### 2.13 Data model (sketch)

All tables except River's carry `tenant_id` and a row-level security
policy.

| Entity          | Key fields                                                                                                                                                                             | Notes                                                                                                                           |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `Tenant`        | slug, settings, limits, managed_by                                                                                                                                                     |                                                                                                                                 |
| `Installation`  | tenant, name, forge kind, host, account, credential kind, secret references, managed_by                                                                                                | one bot on one account; hook path is the name                                                                                   |
| `Repository`    | tenant, installation, forge URI, default branch, settings, enabled, disabled_at, active index run, managed_by                                                                          |                                                                                                                                 |
| `IndexRun`      | tenant, repository, commit SHA, embed model, dims, status, started, finished                                                                                                           | a generation; at most one active per repository                                                                                 |
| `Chunk`         | tenant, repository, index run, path, byte range, language, symbol, kind, signature, scope, vector                                                                                      | `halfvec(KRITIK_EMBED_DIMS)`, HNSW                                                                                              |
| `PullRequest`   | tenant, repository, number, head SHA, author, author is bot                                                                                                                            | head SHA is written by ingest                                                                                                   |
| `Review`        | tenant, pull request, head SHA, merge-base SHA, patch-id, status (`completed`, `superseded`, `skipped`, `capped`, `failed`), model, context record                                     |                                                                                                                                 |
| `Finding`       | tenant, review, path, line, severity, body, forge comment ID                                                                                                                           |                                                                                                                                 |
| `StickyComment` | tenant, pull request, forge comment ID                                                                                                                                                 | cache; the author-plus-marker scan is the source of truth                                                                       |
| `ModelLease`    | tenant, model key, slot, job ID, expires_at                                                                                                                                            | §2.8                                                                                                                            |
| `Usage`         | tenant, repository, review, role, model, input tokens, output tokens                                                                                                                   |                                                                                                                                 |
| `ConfigState`   | applied hash, applied at, leader                                                                                                                                                       | one row, not tenant-scoped, owner-only                                                                                          |
| `RunnerRun`     | tenant, review or index run, kind, Job name, pod, node, phase, created / scheduled / started / finished at, exit code, termination reason, deadline exceeded, log tail, resource usage | one per Kubernetes Job; written by the worker, phase updated by the runner; the log tail expires after 30 days, the row is kept |
| `ContextPack`   | tenant, review, job id, diff, patch-id, stage 1 to 3 chunks with their stage                                                                                                           | written by the runner, read by the worker                                                                                       |

`managed_by` is `file` for every row in v1. v2 adds `dashboard` rows and
the `User`, `TenantMember` and `Session` tables. No secret material is
stored in any table, in either version, except v2's envelope-encrypted
dashboard credentials (§2.17).

### 2.14 Evaluation

Review quality is measured, not judged by impression. v1 ships an offline
evaluation harness behind a `bench` build tag, run with a `mise` task. It
needs live models and takes minutes, so it stays out of the default test
run.

- **Corpus.** Merged PRs from the org's repositories where a later commit
  fixed a defect the PR introduced, plus PRs where a human reviewer left a
  substantive comment. Each case records the repository, the PR's
  merge-base and head, the expected finding (path, line range, a
  one-sentence description), and whether it is a must-find or a
  nice-to-have. v1 starts with 30–50 cases and grows as real reviews miss
  things.
- **Metrics.** Recall on must-find cases. Precision as the share of
  emitted findings a human marks useful, on a sampled subset. Cost and
  latency per review from `Usage`.
- **Baseline.** Robin on the same corpus, run once and stored.
- **Ablations.** Diff only; diff plus overlay, definitions and callers
  (§2.10 stages 1–3); diff plus similar code only; all four stages. The
  retrieval configuration that ships is the one the ablation selects, and
  a stage that does not help is removed.
- **Gate for replacing the action.** The `ai-review` action is retired
  for a repository once the service's must-find recall is at or above
  Robin's and its precision is no more than 10 points below Robin's. The
  pilot is `flate` (§3).

memini's `bench/` package is the precedent for the harness shape:
dataset files, a build tag, and quality and ablation tests run through
`mise`. The harness lives in the kritik repository under `bench/`, runs
only locally with a model key, and never in CI. Cases are added whenever
a real review misses something a human caught.

**As built (2026-09-24).** `bench/cases/*.yaml` hold cases (repository,
head and base commits, expected findings as head-side line ranges with
`must`); `mise run bench` runs `TestBench` behind the `bench` tag with
`OPENROUTER_API_KEY`, over the modes `diff`, `overlay` and `context`
(stages 1 to 3), through the same fetch, context and prompt code the
service uses, and writes a JSON report per run under `bench/results/`
with must-find and nice-to-have recall, findings that land on an
expected range, cost, tokens and latency per mode. `KRITIK_BENCH_DRY=1`
builds every prompt without calling a model. The stage 4 ablation needs
the index and so a database and embedder; it is not in the offline
harness yet. The corpus is mined rather than hand-written first:
`mise run bench-mine` walks the sibling `flate` and `konflate` checkouts,
blames the lines each non-dependency fix commit changed back to the
squash-merged pull request that introduced them, and records that PR
with the fix as the expected finding. Generated, vendored and prose paths
are skipped. Mined expectations start as must-find and are meant to be
curated: a fix does not prove the defect was visible in the diff.
Precision still needs a human label on a sample of emitted findings; the
report lists them per case for that.

### 2.15 Retention

- **Vectors of disabled repositories** are deleted
  `retention.disabledIndexGrace` after `disabled_at` (default 30 days).
  Re-enabling within the grace period reuses the index.
- **Superseded index generations** are deleted when their replacement
  becomes active.
- **Reviews, findings, follow-ups and usage** are kept indefinitely. They
  are small, and they are the only audit trail of what the bot said in
  v1.
- **River job rows** follow River's cleaner defaults.

### 2.16 Repository conventions

A sibling of konflate: module `github.com/home-operations/kritik`,
`cmd/kritik/main.go` as wiring only, `internal/` for everything not
exported, `.mise/config.toml` as the single source of tool versions and
tasks, the shared `lefthook.common.toml`, `.golangci.yml` with konflate's
enable list, `release-please` in `simple` mode without a `v` prefix
stamping `charts/kritik/Chart.yaml`, an OCI Helm chart with `helm-schema`
and `helm-docs` generated files and `helm-unittest` suites, a distroless
static image with no Node stage in v1, and konflate's `AGENTS.md` copied
verbatim.

The chart (`charts/kritik`, built 2026-09-24) has a `roles` map, so a
single `all` Deployment and a split `ingest` plus `worker` topology come
from the same values file; `all` cannot be combined with a split role, and
a worker-capable role requires the owner and runner Secrets at render
time. It ships the Role and RoleBinding for Jobs in the release namespace,
a runner ServiceAccount with no permissions and no token, a NetworkPolicy
for the service pods and one for runner pods (selected by the
`kritik.home-operations.com/role: runner` label the worker puts on them;
egress to DNS, the egress ports and Postgres only), a `runner` values
block for image, deadline and TTL, a webhook Service that selects only
hook-serving pods, a metrics Service for every pod with an optional
ServiceMonitor, Ingress or HTTPRoute for the webhook path, and per-role
PodDisruptionBudgets. Migrations run at startup under the leader lock with
the owner DSN, so no migration Job ships; one remains an option if a
deployment wants schema changes applied before pods roll. The config file
is a chart-managed ConfigMap (or an existing one) mounted without
`subPath`, secrets the file references are mounted through `secretMounts`,
and the three DSNs come from separate Secrets. The chart README's
CloudNativePG example declares the application and runner roles under
`spec.managed.roles`, because the CNPG bootstrap owner owns the database
and would bypass row-level security if used as `KRITIK_DATABASE_URL`. The
private development deployment installs this chart, so every dev loop
exercises it.

Integration tests against Postgres run behind an `integration` build tag,
against a pgvector-enabled Postgres started by the test task. They
include a row-level security suite that connects as the application role
and asserts that every tenant-scoped table returns nothing with the
tenant unset, and nothing of tenant A with tenant B set.

**License.** AGPL-3.0, like every Go project in the org. home-operations
runs a deployment for its own accounts from the same image and chart. It
is not a public service. No dual licensing and no CLA.

### 2.17 v2: dashboard

Deferred, not dropped. v1 prepares for it in three ways: Postgres is the
source of truth, usage and review history are recorded from day one, and
the shared instance App remains possible through environment variables.

v2 adds, in one release:

- A `web` role serving a UI on the same stack as konflate: Svelte 5 with
  runes, Vite, Tailwind, TypeScript, Geist fonts, `@mdi/js` and
  `simple-icons` icons, Playwright end-to-end tests, built into
  `internal/web/dist` and embedded with `go:embed`. The `web` role
  connects with `DATABASE_URL` and is subject to the same startup
  assertion and policies.
- Sign-in by forge OAuth plus generic OIDC, either or both per
  deployment, as ADR-0001 §2.14 describes. Signed session cookies are
  backed by a session table.
- Dashboard-managed tenants, installations and repositories carry
  `managed_by = dashboard`. File-managed rows are read-only there, and a
  slug collision is an error. Credentials entered in the UI are
  envelope-encrypted with a key from the environment.
- Roles: instance operator (an environment-configured allowlist), tenant
  admin, tenant member. State-changing endpoints require a tenant admin.
  Every write is audit-logged.

Whether the dashboard may edit file-managed objects stays a v2 question.
The default answer is no. An instance operator's cross-tenant views run as
the application role by iterating the tenants the operator may see, one
tenant setting per query; the owner DSN is never given to `web`.

Amended by [ADR-0009](0009-web-dashboard.md), which builds this dashboard.

---

## 3. Consequences

### 3.1 Positive

- **The mechanisms work as specified.** Merge-base, job coalescing, model
  concurrency and tenant isolation each have a design that holds under
  concurrent pushes, pooled connections and replica rollouts, not just on
  the happy path.
- **Isolation fails closed and loudly.** A misconfigured DSN stops the
  service at startup instead of silently disabling row-level security.
- **Retrieval starts deterministic.** Definitions and callers come from
  exact lookup, so the most useful context does not depend on embedding
  quality or a similarity threshold.
- **Embedding changes are safe.** A model change can't mix incompatible
  vectors, and reviews stay useful during a reindex.
- **Quality claims are testable.** Retiring the action is a measured
  decision, and later ADRs on prompts and chunking have data to argue
  from.
- **v1 is still konflate-shaped.** Environment plus a file, no auth, only
  `/hooks` and health exposed, one binary, one Postgres.

### 3.2 Negative and trade-offs

- **Two database roles.** Every install provisions an owner and an
  application role, and the chart handles two DSN Secrets. This is more
  setup than one `DATABASE_URL`. The startup assertion makes it hard to
  get wrong, but it is still more to explain.
- **One embedding model per deployment.** A tenant cannot pick its own
  embedder. This is deliberate. A per-tenant embedder means per-dimension
  tables and indexes, and the review model is where tenants' model choice
  matters.
- **Reindexing costs money and time.** Changing the embedding model
  re-embeds every repository, and a dimension change needs an explicit
  recreate.
- **More forge calls per review.** Each review asks the forge for the
  merge-base, which is one extra API call. This is small next to the
  model call.
- **The evaluation corpus is ongoing work.** It has to be built before the
  action can be retired, and kept current as models change.
- **Two copies of the GitHub App transport and sticky-comment logic** are
  kept in sync with konflate by hand, as in ADR-0001.
- **pgvector is a hard requirement.** The homelab story is one binary plus
  a CloudNativePG cluster on a `standard` image.
- **Kubernetes is a hard requirement.** Runner pods buy a trust boundary
  that a long-running worker cannot offer, at the cost of konflate's
  run-anywhere quick start and a few seconds of pod start per job.

---

## 4. Rollout

1. Build indexing, review and write-back against GitHub first, then
   GitLab and Forgejo, with the forge integration table (§2.7) as the
   acceptance list. Fetch by SHA is verified on GitHub as part of this
   step; the other forges are verified when their turn comes.
2. Build the evaluation corpus from `flate` and konflate history in
   parallel, and record Robin's baseline.
3. Point the service at `flate` alongside the action. Both comment.
4. Run the ablation and select the retrieval stages (§2.14).
5. Retire the action on `flate` when the gate passes, then repository by
   repository.

---

## 5. Deferred and open questions

### 5.1 Decided during the rebase

Questions raised while reviewing the published draft, answered here so the
implementation does not have to reopen them:

- The `chunks` table is created by the leader at startup, not by a
  migration, so migrations stay free of environment-dependent DDL (§2.8).
- `all` and `worker` run migrations at startup under the leader lock; the
  chart's migration Job is optional (§2.16).
- Every role except runner loads the file in memory; only the leader
  writes (§2.6).
- Replies on inline findings count as follow-ups, scoped to the finding
  (§2.7).
- An `ignore` list of path globs applies to chunking and the caller search,
  set in `.kritik.yaml` or per repository in the operator's file (§2.6).
- Tokens are counted with a per-provider approximation (§2.10).
- A fallback call releases the primary lease before taking its own (§2.8).
- `RunnerRun` log tails expire after 30 days; rows are kept (§2.13).
- The evaluation harness lives under `bench/`, runs locally only, and the
  corpus starts from `flate` (§2.14).
- The v2 dashboard iterates tenants under the application role; the owner
  DSN never reaches `web` (§2.17).

### 5.2 Still open

- **Default embedding model and dimension.** Choose within the `halfvec`
  4,000-dimension limit. Measure whether a truncated Matryoshka-style
  embedding at 512–1,024 dimensions costs recall on the corpus.
- **Fetch by SHA on GitLab and Forgejo.** GitHub is verified: on
  2026-09-24 go-git fetched two bare SHAs from a public repository at
  depth one and diffed them, with no ref involved (`internal/gitfetch`).
  Real git only advertises the capability when
  `uploadpack.allowReachableSHA1InWant` is set, which GitHub does
  server-side; confirm Forgejo and self-hosted GitLab do with default
  settings. The deepen fallback covers any that do not, at a cost.
- **Chunk text for embedding.** How much of the signature, scope and
  imports is embedded with the chunk versus stored alongside for prompt
  assembly. This belongs to the follow-up prompt ADR and is decided by the
  harness.
- **Vector index engine.** pgvector HNSW with iterative scans is the v1
  decision. If filtered recall falls short on the corpus, compare
  VectorChord.
- **Go 1.27 generic methods in `tree-sitter-go`.** Ship without them.
  Watch upstream PR 198 and re-run the parity check when a grammar
  release lands.
- **Whether the dashboard may edit file-managed objects** (v2).
- **`ADR-0001-review.md`.** Referenced throughout by finding ID; to be
  published alongside this document.

---

## 6. Alternatives considered

Alternatives already recorded in ADR-0001 still stand, and the reasoning
is not repeated here: Redis, RabbitMQ and embedded NATS JetStream for the
queue; agentic exploration instead of an index; official CGO tree-sitter
bindings, a split static and CGO image pair, and tree-sitter compiled to
WASM under wazero. Alternatives added by this ADR:

- **A `ReviewRun` custom resource reconciled by a controller.** It would
  give `kubectl get reviewruns` with status conditions and a declarative
  trigger, at the cost of a second source of truth for state River already
  owns, one high-churn etcd object per review with no reader Postgres does
  not already serve, and a controller-runtime, CRD-versioning and
  cluster-scoped RBAC layer. The domain object is a pull request, not a
  cluster resource. If something outside the service ever needs to request
  or observe reviews in Kubernetes terms, a thin trigger-only
  `ReviewRequest` resource can be added later without changing the runner.
- **Computing the merge-base locally from shallow fetches.** It cannot
  work at depth 1, because merge-base needs ancestry. Deepening until it
  resolves works everywhere, but costs a variable number of round trips on
  every review. It is kept as the fallback, not the default.
- **Per-tenant embedding models.** This needs a table and index per
  dimension, or a column per model, plus per-tenant reindex scheduling.
  The benefit, choosing one's own embedder, doesn't justify it for an
  operator-trusted, operator-run deployment.
- **River unique on the PR, with custom unique states.** River requires
  `running` in any custom unique state list, so a push during a review
  would still be dropped. Head-SHA uniqueness plus staleness checks is the
  only arrangement that loses no push.
- **Session advisory-lock concurrency slots**, as in Kodus. They are
  simple, but each holds a pooled connection for the length of a model
  call. Leases hold nothing and expire on their own after a crash.
- **`FORCE ROW LEVEL SECURITY` with one role.** It would make the single
  role subject to its own policies, but the loader writes across tenants.
  It would then need `BYPASSRLS`, which requires a superuser to grant, or
  a per-row tenant switch. A non-owner application role gives the same
  guarantee with standard privileges.
- **`SET ROLE` from one privileged login instead of two DSNs.** One
  credential, but the process then holds a login that can bypass every
  policy, and an unreset `SET ROLE` on a pooled connection has the same
  leak problem as a session-level tenant setting.
- **Per-repository partitions of `chunks`.** Exact-scope HNSW searches,
  but DDL on the onboarding path and index count growing with repository
  count. Filtered search with iterative scans is simpler at the expected
  scale.
- **VectorChord instead of pgvector HNSW.** memini's choice. It is not in
  CloudNativePG's `standard` images, so it would need a custom image. It
  stays as the first alternative if filtered recall falls short.
- **Per-repository SQLite vector files in object storage.** ADR-0001
  rejected these partly on the grounds that no CGO-free Go path to a
  mature SQLite vector extension exists. That is incorrect.
  `asg017/sqlite-vec-go-bindings` runs sqlite-vec under ncruces' WASM
  driver without CGO, and memini ships it as its default backend. The
  rejection stands on ADR-0001's other grounds: a second `Store` backend
  doubles the tenancy and vector test surface, and it adds object storage
  and a download per review.
- **A hand-written OpenRouter client** (about 200 lines, no dependency)
  and **`openai/openai-go` alone** for both call types. Either would do
  for one provider; Fantasy was chosen because it is the consolidation
  path once it gains embeddings, and its OpenRouter provider already
  exposes routing, fallback and cost.
- **Vector-only retrieval**, as in ADR-0001. Rejected because the most
  useful context for a code change is exact (definitions and callers), and
  memini's experience with prose shows similarity alone needs hybrid
  support even where identifiers matter less.

---

## 7. References

### Predecessor and companion documents

- [ADR-0001](0001-kritik-pr-review-service.md), superseded; also at [gist d5f6acab](https://gist.github.com/onedr0p/d5f6acabf8762754960c68b00ac82577)
- `ADR-0001-review.md`: the review this ADR resolves (not yet published)
- [ADR-0002 as published](https://gist.github.com/perfectra1n/6a67dd0362ea9d7afd24a4e917028581), before this rebase

### External

- [River: unique jobs](https://riverqueue.com/docs/unique-jobs): default unique states and the states required when customising them
- [pgvector](https://github.com/pgvector/pgvector): `halfvec`, HNSW dimension limits, iterative index scans
- [PostgreSQL: row security policies](https://www.postgresql.org/docs/current/ddl-rowsecurity.html) and [`set_config`](https://www.postgresql.org/docs/current/functions-admin.html#FUNCTIONS-ADMIN-SET)
- [CloudNativePG: database role management](https://cloudnative-pg.io/documentation/current/declarative_role_management/)
- [`odvcencio/gotreesitter`](https://github.com/odvcencio/gotreesitter)
- [`eleboucher/memini`](https://github.com/eleboucher/memini): embedding-model handling (`MEMINI_REEMBED_ON_MODEL_CHANGE`), hybrid retrieval, the `sqlitevec` store, the `bench/` harness
- [`kodustech/kodus-ai`](https://github.com/kodustech/kodus-ai)

---

## Appendix A: tree-sitter parity study

Carried unchanged from ADR-0001 §2.9. Run 2026-09-24 against every
home-operations repository checked out locally (24 repositories, 2,805
detected files, gotreesitter v0.54.0 with all 206 grammars embedded,
strict parsing with a 10-second timeout). No file stopped early on a
safety cap or timeout, and no parse call failed, in any language. Files
whose tree contained syntax error nodes:

| Language                                                                     | Files | With error nodes | Cause                                                                                                                                                    |
| ---------------------------------------------------------------------------- | ----- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Go                                                                           | 1,072 | 2                | Methods with their own type parameters, added in Go 1.27; the upstream `tree-sitter-go` grammar has no slot for them, so the C runtime fails identically |
| YAML                                                                         | 707   | 0                |                                                                                                                                                          |
| Rust                                                                         | 272   | 0                |                                                                                                                                                          |
| Markdown                                                                     | 219   | 0                |                                                                                                                                                          |
| JSON, TOML, Bash, JSON5, HCL, JavaScript, TypeScript, Svelte, Ruby, `go.mod` | 415   | 0                |                                                                                                                                                          |
| Dockerfile                                                                   | 50    | 2                | A comment inside a backslash continuation (open upstream issue) and a bash `[[ =~ ]]` regex inside `RUN`; both grammar-level                             |
| Misc config formats by extension (`.conf`, `.cfg`, `.txt`, `.j2`)            | 24    | 8                | Wrong grammar guessed; excluded by the chunking allowlist                                                                                                |

Throughput on one core was roughly 20 ms per Go file and 26 ms per Rust
file. In the two affected Go files, the parser resynchronises at the next
declaration: 43 of 46 and 6 of 10 top-level declarations parse cleanly,
and only the generic methods take the fixed-size path.

---

## Appendix B: section map from ADR-0001

| ADR-0001                                                            | ADR-0002               | Change                                                                                            |
| ------------------------------------------------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------- |
| 1.1–1.3                                                             | 1.1–1.3                | Condensed, unchanged in substance                                                                 |
| 1.4 Kodus                                                           | 1.4                    | Summarised, memini added                                                                          |
| 1.5 Origin                                                          | —                      | Removed (E3)                                                                                      |
| 2.1 Goals                                                           | 2.1                    | Measured quality added                                                                            |
| 2.2 Non-goals                                                       | 2.2                    | Per-tenant embedding models added as a non-goal                                                   |
| 2.3 Architecture                                                    | 2.3                    | Merge-base, staleness checks, leases, retrieval stages                                            |
| 2.4 Tenancy                                                         | 2.4                    | Two roles, transaction-local tenant, startup assertion, secret re-reads (B5, S6)                  |
| 2.5 Reuse                                                           | 2.5                    | `MergeBase` added to providers; coalescing pattern replaced (B1, B3)                              |
| 2.6 Configuration                                                   | 2.6                    | Embedding moved to environment; leader-only loader; retention block (B2, S5, S9)                  |
| 2.7 Forge surface                                                   | 2.7                    | Merge-base, head ref and bot identity rows; loop guard; patch-id rule (B1, S7)                    |
| 2.8 Storage                                                         | 2.8                    | Head-SHA jobs, leases, `halfvec` schema, model-change handling, index shape (B2, B3, S1, S4)      |
| 2.9 Git and indexing                                                | 2.9                    | Forge merge-base, index generations, symbol metadata; parity study to Appendix A (B1, B2, E1, E2) |
| —                                                                   | 2.10 Context retrieval | New (S2, S3)                                                                                      |
| 2.10 Pipeline                                                       | 2.11                   | Staleness steps, poll interval, write-back transaction                                            |
| 2.11 Model adapters                                                 | 2.12                   | `Completer` and `Embedder` (B4)                                                                   |
| 2.12 Data model                                                     | 2.13                   | Generations, symbol metadata, leases, review statuses, config state                               |
| —                                                                   | 2.14 Evaluation        | New (S8)                                                                                          |
| —                                                                   | 2.15 Retention         | New (S9)                                                                                          |
| 2.13 Conventions                                                    | 2.16                   | Migration Job, two DSNs, CNPG role example, RLS test suite                                        |
| 2.14 v2                                                             | 2.17                   | `web` role subject to row-level security                                                          |
| 3 Consequences                                                      | 3                      | Rewritten as positive and negative                                                                |
| 2.3, 2.9, 2.12 (runner, no CRD, `RunnerRun`, added after 16:39 UTC) | 2.3, 2.9, 2.10, 2.13   | Carried in the rebase; retrieval split across runner and worker                                   |
| —                                                                   | 4 Rollout              | New                                                                                               |
| 4 Open questions                                                    | 5                      | Embedding dimension, fetch by SHA, vector engine added                                            |
