# ADR-0001: kritik, a multi-tenant AI pull request review service

- **Status:** Superseded by [ADR-0002](0002-kritik-pr-review-service.md); kept unchanged as historical input
- **Date:** 2026-09-24
- **Builds on:** [`home-operations/konflate`](https://github.com/home-operations/konflate) (forge, git, webhook and PR-filter code, copied), the `ai-review` composite action on the `feat/ai-review` branch of [`home-operations/.github`](https://github.com/home-operations/.github) (the org's current PR review, wrapping [`antongulin/robin`](https://github.com/antongulin/robin)).
- **Prior art examined:** [`kodustech/kodus-ai`](https://github.com/kodustech/kodus-ai) and [`kodustech/kodus-installer`](https://github.com/kodustech/kodus-installer) (AGPLv3, hosted and self-hosted, multi-tenant). Mira and Greptile by their public docs only.

> Scope: this ADR covers product shape, tenancy, the reuse boundary with
> konflate, storage, process roles, the configuration file, and the forge
> integration surface. Package layout, prompt design, and chunking and
> embedding strategy belong to follow-up ADRs once this shape is agreed.
>
> Decisions settled on 2026-09-24 and recorded below: v1 is headless and
> driven by a declarative configuration file, the dashboard is v2 (2.3,
> 2.14); embedding index for context (2.9) stored in pgvector, which is a
> hard requirement (2.8); forge code copied from konflate rather than
> shared (2.5); a GitHub App or bot token is a credential a tenant can own,
> so each account can have its own bot (2.4); follow-ups by @-mentioning
> the bot (2.10); AGPL-3.0, with home-operations running a deployment for
> its own accounts only and no public service (1.3, 2.15); v2 sign-in by
> forge OAuth plus generic OIDC (2.14); AST chunks via tree-sitter through
> the pure-Go `gotreesitter` runtime with all 206 grammars embedded,
> keeping the static build, verified at parity on the org's own
> repositories (2.9); row-level security from the first migration (2.4);
> no in-band version field in the config file (2.6); every index and
> review job runs in its own Kubernetes Job pod that holds no secrets,
> making Kubernetes a hard requirement (1.3, 2.3, 2.9); the project is
> named **kritik** (German and Turkish for "critique"), giving the module
> `github.com/home-operations/kritik`, the `KRITIK_` environment prefix,
> the `<!-- kritik:... -->` comment marker and the `.kritik.yaml` in-repo
> file.

---

## 1. Context

### 1.1 What the org has today

- **A GitHub-only, CI-shaped review.** `actions/ai-review` runs Robin inside
  a workflow: it reads the PR diff through the GitHub API, sends it to an
  OpenAI-compatible endpoint (OpenRouter by default), and posts a summary
  comment plus inline comments. It has taught two lessons the service must
  keep: a `REQUEST_CHANGES` review from a bot blocks auto-merge and evicts
  merge-queue entries, so findings are posted as a plain comment; and
  bot-authored PRs are reviewed once on open, because every Renovate rebase
  is a `synchronize` event on an unchanged diff.
- **No repository context.** Robin sees the diff and nothing else.
- **No GitLab or Forgejo path.** A GitHub Actions workflow cannot run for
  the self-hosted forges the org's own tools (konflate, flate) support.
- **A mature forge layer next door.** konflate lists, clones, renders and
  writes back to PRs on GitHub, GitLab and Forgejo, cloud or self-hosted,
  from one Go binary. It is single-repository per process, configured by
  environment variables, has a read-only HTTP surface, and keeps everything
  under `internal/`.

### 1.2 Why a service and not a bigger action

A repository-aware review needs an index that outlives one CI run, a
sticky comment identity, review history, and a follow-up channel. A
workflow job starts cold every time and cannot run on GitLab or Forgejo.

### 1.3 Deployment shape

One shape: **an operator runs one deployment for one or many forge
accounts they own**, where an account is a GitHub organisation or personal
user account, a GitLab group, or a Forgejo organisation. home-operations
runs it for its own accounts; anyone else runs their own deployment with
their own bots. There is no public service that strangers sign up to, and
no GitHub App is run on others' behalf.

**Kubernetes is the runtime.** Every index and review job runs as a
Kubernetes Job in its own pod (2.3), so the service requires a cluster
and RBAC to create Jobs. There is no compose or VM install; a homelab is
a cluster with CloudNativePG, which is the org's own shape. An in-process
executor exists for development and tests only.

Consequences:

- Tenants are **declared by the operator** in a configuration file, not
  created by sign-up. A shared GitHub App must be public to install across
  accounts, and a public App can be installed by anyone, so an installation
  from an undeclared account is logged and ignored. A tenant with its own
  private App avoids the exposure entirely.
- A homelab with one account is the same deployment with one tenant.
  Tenancy is always on; there is no single-tenant code path to keep in
  sync.
- Every tenant is operator-trusted. Guards against untrusted input (forks,
  egress) stay, as configurable defaults rather than hosted-mode rules.

### 1.4 What Kodus does, verified from its repositories

Kodus is the closest open-source peer in shape and was read for this ADR.
Its runtime is six services (web, api, webhooks, worker, an optional
analytics worker, and an MCP manager) plus RabbitMQ, Postgres with
`pgvector`, and MongoDB. Findings that inform the decisions below:

- **No Redis anywhere.** Delivery is RabbitMQ; ownership, idempotency and
  locking are Postgres: an inbox claim via atomic upsert on
  `(consumerId, messageId)` with a lock timeout, an outbox relay polling
  with `FOR UPDATE SKIP LOCKED`, and advisory locks for the per-tenant
  concurrency gate.
- **Per-tenant LLM concurrency is N advisory-lock slots** per organisation
  and model. A job that finds no free slot goes back to pending with a
  future attempt time and exponential backoff capped at five minutes.
- **No persistent git mirror.** Every review runs in an ephemeral sandbox
  (E2B or a local temp dir) with a depth-1 fetch of the PR ref and the base
  branch. Their internal AST plan records that building repo context inside
  that sandbox times out on large repos, and proposes building the index
  once at onboarding, storing it, and updating it incrementally on
  default-branch pushes.
- **Webhook ingest is a separate process** that only verifies and
  enqueues, scaled independently of workers.

### 1.5 Origin of this document

This proposal started as a design for a different organisation with a
different stack (SvelteKit on `@immich/ui`, Postgres-or-SQLite, a
dashboard with a built-in OIDC client) and a single-tenant v1. Section 2
keeps the product goals and reworks the rest for multi-tenancy and for
how home-operations projects are built and operated.

---

## 2. Decision

### 2.1 Goals

- Whole-repo-aware review: the model sees relevant code from an index of
  the repository, not only the diff.
- One sticky summary comment per PR, edited in place on every push, plus
  inline findings where the forge supports line anchoring.
- Conversational follow-ups in the PR thread.
- GitHub, GitLab and Forgejo from day one, cloud and self-hosted.
- Multi-tenant from day one: tenant-scoped data, credentials, settings,
  concurrency limits and usage accounting; a bot per tenant if wanted.
- Declarative: everything the service manages is declared in one file the
  operator keeps in git, so a Flux-managed cluster manages the service the
  way it manages everything else.
- One image, one binary, role-selectable at start. The only external
  dependency is Postgres with the `pgvector` extension.
- Bring-your-own-model per tenant (OpenAI-compatible endpoint or Anthropic
  direct), with an operator-provided default.

### 2.2 Non-goals

- **v1:** a dashboard or any authenticated HTTP surface. The PR thread,
  logs and metrics are the surfaces. Section 2.14 describes the v2
  dashboard and what v1 does so it is additive.
- A public hosted service, sign-up, billing or licensing. Usage is
  recorded per tenant for cost visibility, not for charging.
- IDE or CLI surfaces.
- Replacing konflate. Section 2.10 describes the one integration point.

### 2.3 High-level architecture

```text
config file (tenants, credentials, repositories, models)
   -> loader: validate whole file, compile filters, resolve secret refs,
              upsert into Postgres as file-managed rows; reload on change

forge webhook  ->  ingest role: look up secret by /hooks/{installation},
                   verify signature, enqueue. Never does work.

job queue (River, Postgres)
   -> index job      onboarding, or push to default branch
        -> worker creates a Kubernetes Job (same image, --role runner)
             pod: shallow fetch -> parse -> chunk -> write chunks to Postgres
        -> worker: embed chunks -> pgvector
   -> review job     PR opened / pushed / @-mention
        -> tenant concurrency gate (advisory-lock slots), defer if full
        -> worker creates a Kubernetes Job
             pod: fetch head and merge-base -> diff -> changed declarations
                  -> context pack written to Postgres
        -> worker: retrieve chunks, model call, fallback model on error
        -> worker: write-back: sticky comment, inline findings, check status

health and metrics on a separate port, as konflate does
```

One image with a `--role` flag: `all`, `ingest`, `worker`, `runner` in
v1, `web` added in v2. A homelab runs `all` with one replica; the runner
pods it spawns are the same image. A larger deployment runs `ingest` and
`worker` as their own Deployments with their own replica counts and HPA.

**Trust boundary.** The pod does everything that touches repository
contents: clone, parse, chunk, diff. It holds a short-lived git read
token for one repository and nothing else: no model key, no forge write
credential, no database role beyond writing its own job's rows. The
long-running worker keeps the model call, the concurrency gate and the
write-back. This is what Kodus buys with an E2B sandbox per review and
kopiur with a pod per backup: a fork's submodules, hooks and build
scripts run in a container that owns no secrets, with its own
NetworkPolicy and resource limits, and the worker serving other tenants
cannot be taken down by one huge repository.

The HTTP surface in v1 is `/hooks/{installation}` on the ingest role and
`/healthz`, `/readyz`, `/metrics` on the management port. Nothing else.
Writes to forges come only from the worker, using credentials held by
that process, never from a request or a runner pod: konflate's security
property, kept and narrowed.

### 2.4 Tenancy and credentials

```text
Tenant  1..n  Installation (with its Credential)  1..n  Repository  1..n  PullRequest  1..n  Review
```

- **Tenant** is a forge account and the unit of isolation, settings,
  credentials, limits and usage accounting. Declared in the configuration
  file (2.6).
- **Installation** is one bot on one account: a GitHub App installation,
  or a GitLab or Forgejo bot with a group or org token. Each installation
  owns its credential and its webhook secret, and has its own hook URL,
  `/hooks/{installation-name}`, so the ingest role finds the right secret
  before it trusts a payload.
- **A GitHub App is a credential, not instance configuration.** Each
  tenant can bring its own App, private and installed only on itself, with
  its own permissions, name and avatar, revocable by that account's
  admins. An instance-level shared App, configured by environment
  variables, is an optional default for tenants that do not bring one.
  Mixed deployments are fine.
- **No secret material is stored.** Every credential in the file is a
  reference to a file or an environment variable, resolved at load and
  held in process memory. Postgres holds references and metadata only. The
  Helm chart mounts Kubernetes Secrets as files into the worker. Runner
  pods receive only a short-lived git token for their one repository,
  passed as an environment variable on the Job, and a database
  connection scoped to a runner role that can write chunks and context
  packs for its own job id and read nothing else. The worker's use of the
  Kubernetes API is limited to creating, watching and deleting Jobs in
  its own namespace.
- **Every table carries `tenant_id`, enforced twice.** The `Store` layer
  scopes every query by tenant; there is no unscoped query API. Postgres
  row-level security is enabled from the first migration as defence in
  depth: every tenant-scoped table has a policy keyed on a session
  variable the `Store` sets when it checks out a connection for a request
  or job, so a query that forgets the tenant filter returns nothing rather
  than another tenant's rows. The loader and migrations run as a role the
  policies bypass.
- **Usage accounting:** one row per model call with tenant, repository,
  review, role (review, indexing, fallback), model, input and output
  tokens, exported as metrics per tenant and repository. Optional
  per-tenant caps on reviews per day and tokens per month are enforced in
  the review worker before the model call; unset by default.

Worked example: one instance serving the `home-operations` organisation
through an App named `sticky-gecko` and the `onedr0p` personal account
through an App named `bot-ross`. Reviews on home-operations repositories
are posted as `sticky-gecko[bot]`, on onedr0p repositories as
`bot-ross[bot]`. Both Apps stay private. The file for it is in 2.6.

### 2.5 Reuse boundary with konflate

konflate's forge code is **copied into this repository, not shared**. No
new module, no change to konflate. The duplication is accepted because
the two services diverge immediately: konflate is one repository per
process with process-wide credentials, this service is many repositories
per tenant with per-installation credentials, and a shared module would
have to carry both shapes from its first release. If the copies converge
again later, extraction is a follow-up.

| konflate package                                                  | Copy                                                            | Change on arrival                                                                                                                                                              |
| ----------------------------------------------------------------- | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `internal/gitclone`                                               | as-is                                                           | runs inside the runner pod, which is ephemeral by construction. The persistent bare mirror is dropped; keep the depth-1 fetch of head and base, merge-base and tree extraction |
| `internal/prfilter`                                               | as-is                                                           | none; supply this service's own `pr` variable map                                                                                                                              |
| `internal/webhook`                                                | with the forge-kind enum                                        | secret lookup by installation name from the hook path. Add comment-created events (2.7)                                                                                        |
| `ForgeURI`, `ParseForgeURI` from `internal/config/forge.go`       | as-is                                                           | none                                                                                                                                                                           |
| `internal/provider` (`provider.go`, `writer.go`, per-forge files) | with the GitHub App JWT and installation-token transport intact | constructors take an options struct per installation instead of `*config.Config`; this service's own `PR` type; add inline review comments (2.7)                               |
| `internal/config` pattern                                         | as a pattern                                                    | one `Config` struct via `caarlos0/env` for process configuration, with the same `,unset` scrubbing of secrets                                                                  |

Copied as patterns, not code: the per-PR coalescing queue (replaced by
River's unique-job constraint keyed on tenant, repo and PR), the keyed
write mutex (replaced by a Postgres advisory lock per PR held only during
write-back), and the retry with backoff.

The GitHub App JWT and installation-token transport, roughly 110 tested
lines, is the piece most worth copying verbatim and keeping in sync with
konflate by hand when either side fixes a bug in it.

### 2.6 Configuration

Two sources, one direction:

- **Environment variables** for process configuration, konflate-style:
  database URL, role, listen addresses, the config file path, the optional
  instance-level shared App, the default model provider key, the egress
  guard, cache directory. Secrets `unset` after load.
- **The configuration file** for everything the service manages: model
  providers, defaults, tenants, installations, repositories. Mounted from
  a ConfigMap and reloaded on change.

```yaml
# Model endpoints. Keys are references, never values.
providers:
  openrouter:
    type: openai # openai-compatible
    baseUrl: https://openrouter.ai/api/v1
    apiKey: { env: OPENROUTER_API_KEY }
  anthropic:
    type: anthropic
    apiKey: { file: /var/run/secrets/anthropic/api-key }

# Applied to every tenant unless overridden. Model names illustrative.
defaults:
  models:
    review: openrouter/openai/gpt-6-sol
    indexing: openrouter/<embedding-model>
    fallback: anthropic/claude-sonnet-5
  filter: "!pr.draft"
  forks: false
  limits:
    concurrency: 2 # advisory-lock slots per tenant and model

tenants:
  - slug: home-operations
    runner: # optional; chart defaults otherwise
      resources: { limits: { cpu: "2", memory: 4Gi } }
      activeDeadlineSeconds: 900
    installations:
      - name: sticky-gecko # hook URL: /hooks/sticky-gecko
        forge: github # host defaults to github.com
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
      - name: bot-ross # hook URL: /hooks/bot-ross
        forge: github
        account: onedr0p # personal account
        app:
          clientId: Iv1.yyyyyyyy
          privateKey: { file: /var/run/secrets/bot-ross/private-key.pem }
          webhookSecret: { file: /var/run/secrets/bot-ross/webhook-secret }
      - name: onedr0p-forgejo
        forge: forgejo
        host: git.example.org # self-hosted
        account: onedr0p
        token: { file: /var/run/secrets/forgejo-bot/token }
        webhookSecret: { file: /var/run/secrets/forgejo-bot/webhook-secret }
    models:
      review: anthropic/claude-opus-5
    forks: true
```

Rules:

- **Secret references are `env` or `file` only.** No vault client, and no
  reading Secrets through the Kubernetes API; the chart mounts them. What
  mounts the file is the operator's business.
- **Repositories are opt-out.** Everything an installation grants access
  to is watched. The `repositories` list carries overrides and
  `enabled: false`. For GitHub, repository selection happens in the App's
  installation settings, where the account's admins already expect it.
- **Settings resolve in one direction:** `defaults`, then tenant, then
  repository, then the in-repo file (below), which can only narrow. It can
  tighten the filter or disable review; it cannot enable forks, raise a
  limit or change a model.
- **The file is applied atomically.** On reload the whole file is
  validated first: every CEL filter compiled and smoke-tested against a
  sample PR, every reference resolved, every installation name unique. A
  bad file is rejected with the error logged and the last good state stays
  live.
- **The file is not versioned in-band.** No `apiVersion` field. Like
  konflate's environment variables, the schema is documented, validated
  strictly (unknown keys are errors), and changed with release notes; a
  breaking change is a breaking release.
- **The loader upserts into Postgres.** Tenants, installations and
  repositories become rows marked `managed_by = file`. Rows absent from
  the file after a reload are disabled, not deleted, so history and index
  survive a mistake. Postgres is the source of truth for the workers even
  though the file is the only writer in v1; this is what makes the v2
  dashboard additive.

The in-repo file, `.kritik.yaml` at the repository root, uses the same
vocabulary restricted to what may narrow:

```yaml
filter: '!pr.labels.exists(l, l.name == "wip")'
enabled: true
```

### 2.7 Forge integration surface

Inherited from konflate:

|                   | GitHub                                                                                                                                                          | GitLab                                      | Forgejo                                           |
| ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------- | ------------------------------------------------- |
| Install           | GitHub App, the tenant's own or the instance's shared one; the installation webhook attaches the installation to the declared tenant and syncs its repositories | Bot user plus group or project access token | Bot account plus token                            |
| API and git auth  | Installation token, minted and refreshed per installation from whichever App owns it                                                                            | Token                                       | Token                                             |
| Sticky comment    | Matched by **author id plus hidden marker**, never marker alone, so a PR author cannot hijack it                                                                | Note matched by author plus marker          | Comment matched by poster plus marker             |
| Status            | Check Run (`success` / `neutral`), falling back to commit status                                                                                                | Commit status                               | Commit status                                     |
| Webhook           | `X-Hub-Signature-256` HMAC, one secret per App                                                                                                                  | `X-Gitlab-Token`, per-installation secret   | `X-Gitea-Signature` HMAC, per-installation secret |
| Self-hosted forge | GHES via `github://host/owner/repo`                                                                                                                             | `gitlab://host/group/repo`                  | `forgejo://host/org/repo`                         |

Added for this service:

- **Inline review comments** as an optional writer capability, like
  konflate's `CheckRunner`: GitHub Pull Request Review Comments, GitLab
  Discussions with a position, and on Forgejo a pull review submitted
  with state `COMMENT` carrying positioned comments (path, old or new
  line, body, commit id). The Forgejo SDK konflate depends on
  (`forgejo-sdk/forgejo/v3`) exposes exactly that, so all three forges
  get line-anchored findings in v1. On every forge the review state is
  `COMMENT`, never `REQUEST_CHANGES`.
- **Comment-created events** (`issue_comment`, `note`) parsed into a
  `Comment` event with PR number, author and body. A follow-up is
  triggered by **@-mentioning the bot**. The handle differs per
  installation (a GitHub App comments as `<app-slug>[bot]`, GitLab and
  Forgejo as the bot user), so the mention is matched against the same
  resolved bot identity the writer uses to find its sticky comment, never
  a hard-coded name. Only authors with write access or higher trigger a
  follow-up.
- **Egress guard.** Runner pods get a NetworkPolicy from the chart that
  allows the forge host, the git remote and Postgres and nothing else,
  which is the real guard. konflate's `RESTRICT_EGRESS` (private,
  loopback, link-local and metadata ranges blocked, `https` and `ssh`
  only) is carried over inside the runner as defence in depth for
  clusters without a NetworkPolicy implementation. With the pod boundary
  in place, enabling forks for a tenant is a cost decision rather than a
  security one, though it still defaults to off.

Bot behaviour carried over from the action: never `REQUEST_CHANGES`, never
block a merge, review bot-authored PRs once on open. The service enforces
the last rule itself by skipping a `synchronize` on a bot-authored PR whose
diff is unchanged against the last reviewed merge-base.

### 2.8 Storage, queue and coordination

Postgres only, with `pgvector` as a hard requirement. No SQLite mode, no
Redis, no broker.

`pgvector` is required rather than optional because the fallback would be
pulling a repository's entire vector set into the worker for an in-process
scan on every review, tens of megabytes per review at monorepo scale,
versus the index returning a handful of rows. It is the only Postgres
extension the service uses. Enforcement: the worker role checks
`pg_extension` at startup and refuses to start without it, so a
misconfigured database fails at boot, not at the first review. kritik
does not create the extension itself, because that needs superuser and
the application role deliberately is not one. On CloudNativePG the
declarative `Database` resource does it (`spec.extensions: [{name:
vector, ensure: present}]`), which the operator applies as superuser;
this is verified working in `deploy/dev`. Elsewhere the operator
pre-creates it. CloudNativePG's `standard` images bundle it; its
`minimal` images do not, and the chart's `values.yaml` and README say so
next to `DATABASE_URL`.

| Concern                    | Mechanism                                                                                                                       |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| Relational state           | Postgres                                                                                                                        |
| Vectors                    | `pgvector` with an HNSW index, partitioned by tenant and repository                                                             |
| Job queue                  | River: durable, `FOR UPDATE SKIP LOCKED`, per-queue worker limits, unique jobs, snooze for deferral                             |
| Webhook idempotency        | River unique-job key on forge delivery id                                                                                       |
| Per-tenant LLM concurrency | Kodus's design: N advisory-lock slots per tenant and model key; a job that gets no slot snoozes with capped exponential backoff |
| Per-PR write serialisation | advisory lock per PR held only during write-back                                                                                |
| Config reload coordination | file-managed rows carry the file's content hash; every replica reloads independently and converges                              |

Why SQLite is gone: a second `Store` backend doubles the test surface for
tenancy scoping and vector search, and the single-account target is served
well enough by one binary plus a Postgres the operator already runs
(CloudNativePG in the org's own clusters).

**Alternatives considered for the queue and coordination**

- **Redis.** Every role it would play (queue, locks, rate limiting, pub/sub,
  cache) has a Postgres mechanism in the table above. Kodus reaches
  multi-tenant scale without it; its per-tenant concurrency gate is
  advisory locks there and here. If advisory-lock churn ever shows up in
  measurements, a Redis-backed gate is a contained swap behind the same
  interface.
- **RabbitMQ**, as Kodus uses. A broker moves messages but cannot own job
  state, which is why Kodus needed an inbox and outbox layer on top of it.
  A Postgres-native queue enqueues the job in the same transaction as the
  PR row and has no dual write to reconcile.
- **Embedded NATS JetStream.** Possible, but it makes worker pods stateful
  RAFT members with per-pod disks, and growing from one to three replicas
  is a cluster-formation procedure plus a stream replication change, not a
  Helm value. It would sit beside Postgres, not replace it. Job rate here
  is LLM-bound and far below anything that would justify it.

**Alternatives considered for the vector index**

- **Per-repository SQLite file in object storage**, one file per repo,
  quantised, downloaded per review. Attractive for tenant isolation and a
  small primary database, and the shape Kodus's own AST plan takes. Rejected
  for v1 because it adds an S3-compatible store as a dependency and a
  download per review, and because the mature extension for it
  (`sqliteai/sqlite-vector`) is a C extension with no Go binding: its
  maintainers' answer to a Go request (issue 21) is to load the `.so`
  through the CGO driver, which the org's `CGO_ENABLED=0` distroless builds
  cannot do. The CGO-free driver `ncruces/go-sqlite3` instead wraps
  SQLite's own vec1 extension, which is version 0.7 and self-described as
  under-tested. Revisit when index volume per tenant makes the primary
  database the problem; the chunk schema is kept flat (path, byte range,
  vector) so an export to a per-repo file needs no re-embedding.

### 2.9 Git and indexing

**Context strategy: an embedding index.** The review model receives the
diff plus chunks retrieved from a pre-built index of the repository. The
alternative, agentic exploration where the model is given tools over an
ephemeral checkout and no index exists, was considered and not chosen for
v1: it is always current and needs no embedding model, but costs more
tokens per review and makes review latency and cost far less predictable
per tenant. A hybrid that seeds from the index and lets the model verify
with tools is the natural follow-up once the index is proven.

- **Git is a pod per job.** The worker creates a Kubernetes Job from the
  same image with `--role runner`, `ttlSecondsAfterFinished` set, a
  resource limit sized per tenant, and an `activeDeadlineSeconds` that
  bounds a runaway clone or parse. The pod fetches head and base at depth
  1, computes the merge-base, parses and chunks or diffs, writes its
  output rows to Postgres under its job id, and exits; the worker watches
  the Job and continues from those rows. The worker also records each Job
  as a `RunnerRun` row (2.12): pod and node, phase timestamps, exit code
  and termination reason, and the last 64 KB of stdout and stderr fetched
  once at completion, so nothing about a run is lost when the TTL deletes
  the Job. Nothing owns a repository between jobs. A per-node image cache is the only warm state, and the
  image is about 35 MB. An in-process executor implements the same
  interface for `go test` and local development, and is not a supported
  deployment mode.
- **Plain Jobs, no CRD.** Each Job carries labels for tenant, repository,
  PR number and kind (`index` or `review`) and annotations for the River
  job id and head SHA, so `kubectl get jobs -l tenant=<slug>`, per-job
  logs and events, and a Grafana panel on those labels come for free.
  Postgres stays the only source of truth for job state and the worker
  the only writer.

  _Alternative considered:_ a `ReviewRun` custom resource reconciled by a
  controller. It would give `kubectl get reviewruns` with status
  conditions and a declarative trigger, at the cost of a second source of
  truth for state River already owns (pending, running, deferred,
  attempts, per-PR uniqueness), one high-churn etcd object per review
  with no reader Postgres does not already serve, and a whole layer of
  controller-runtime, CRD versioning and cluster-scoped RBAC. The domain
  object is a pull request, not a cluster resource; kopiur has `Backup`
  CRs because PVCs and snapshots are referenced from other manifests, and
  nothing will reference a review that way. Triggers are already covered
  by the webhook, the poll backstop and @-mentions. If something outside
  the service ever needs to request or observe reviews in Kubernetes
  terms, a thin trigger-only `ReviewRequest` resource, in the spirit of
  Tekton's `PipelineRun`, can be added later: the worker would consume it
  into a River job and the runner would not change.

- **The index is the persistent thing.** Built once at onboarding from the
  default branch, stored in Postgres keyed by tenant, repository and
  commit SHA, and updated incrementally on default-branch pushes by
  re-chunking only changed paths. This is the shape Kodus's own plan
  arrives at after measuring that building context inside the review
  sandbox times out on large repositories.
- **Chunks are AST nodes via tree-sitter.** Functions, methods, types and
  top-level declarations become chunks, each carrying its signature, its
  enclosing scope and its file path as metadata. The fixed-size chunker
  covers languages without a grammar, files that stop early on a safety
  cap, and, **per declaration rather than per file**, the byte ranges of
  top-level nodes that contain error nodes; every clean declaration in
  the same file keeps its AST chunk. Kodus's benchmark measured call-graph context lifting review F1
  from 0.338 to 0.464 when the graph built in time; chunking at
  declaration boundaries is the part of that gain that fits an index.
  Callers and callees are not extracted in v1; the chunk metadata leaves
  room for it.
- **Parsing is pure Go, so the static build stays.** Upstream tree-sitter
  is C and every official binding is CGO, which the org's `CGO_ENABLED=0`
  distroless builds cannot link. The service instead uses
  [`odvcencio/gotreesitter`](https://github.com/odvcencio/gotreesitter),
  a pure-Go reimplementation of the runtime (parser, lexer, query engine,
  incremental reparse, external scanners) that loads parse tables
  extracted from upstream grammars. Verified for this ADR: MIT, first
  commit February 2026, v0.54.0 on 2026-09-23 with releases every one to
  two weeks, 206 grammars, and hand-ported Go external scanners for every
  language in the org's own repositories (Go, Rust, TypeScript, TSX, YAML,
  Bash, TOML, Dockerfile, HCL, Python, JavaScript; JSON needs none).
  Queries with the standard predicates are supported. Build tags embed
  only the grammars shipped, so a small language set is a few megabytes
  rather than the roughly 24 MB of the full registry.
- **The parser is isolated behind an interface.** gotreesitter is a
  zero-point-x project with effectively one author, self-reported
  per-grammar parity, and parser safety caps that can fail very large or
  deeply nested files. The chunker therefore talks to a small `Parser`
  interface with two implementations from day one: gotreesitter and the
  fixed-size fallback. A parse that hits a cap or fails outright falls
  back for the whole file; a parse that succeeds with error nodes falls
  back only for the affected top-level declarations. If the project
  stalls, the official CGO bindings can be added as a third
  implementation with a build-mode change rather than a redesign.
- **Parity check, run 2026-09-24 against every home-operations repository
  checked out locally** (24 repos, 2,805 detected files, gotreesitter
  v0.54.0 with all 206 grammars embedded, strict parsing with a 10 s
  timeout). No file stopped early on a safety cap or timeout and no parse
  call failed, in any language. Files whose tree contained syntax error
  nodes:

  | Language                                                                     | Files | With error nodes | Cause                                                                                                                                                                                                                    |
  | ---------------------------------------------------------------------------- | ----- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
  | Go                                                                           | 1,072 | 2                | methods with their own type parameters, added in Go 1.27; the upstream `tree-sitter-go` grammar's `method_declaration` rule has no type-parameter slot as of its latest release, so the C runtime would fail identically |
  | YAML                                                                         | 707   | 0                |                                                                                                                                                                                                                          |
  | Rust                                                                         | 272   | 0                |                                                                                                                                                                                                                          |
  | Markdown                                                                     | 219   | 0                |                                                                                                                                                                                                                          |
  | JSON, TOML, Bash, JSON5, HCL, JavaScript, TypeScript, Svelte, Ruby, `go.mod` | 415   | 0                |                                                                                                                                                                                                                          |
  | Dockerfile                                                                   | 50    | 2                | a comment line inside a backslash continuation (open upstream issue) and a bash `[[ =~ ]]` regex inside `RUN`; both grammar-level                                                                                        |
  | Misc config formats detected by extension (`.conf`, `.cfg`, `.txt`, `.j2`)   | 24    | 8                | wrong grammar guessed for `.conf` and `.txt`; irrelevant to code review and excluded by the chunker's language allowlist                                                                                                 |

  Throughput on one core was roughly 20 ms per Go file and 26 ms per Rust
  file, fast enough that a full index of the largest org repository is
  seconds of parse time. Undetected files were templates (`.tpl`,
  `.gotmpl`), ignore files and lockfiles, which get the fixed-size
  chunker. Conclusion: gotreesitter is at parity with the C runtime on
  this corpus, and the gaps found are upstream grammar gaps. The one that
  matters, Go 1.27 generic methods, is confined: in the two affected
  files the parser resynchronises at the next declaration, so 43 of 46
  and 6 of 10 top-level declarations still parse cleanly and only the
  generic methods themselves take the fixed-size path. Upstream has an
  open pull request (`tree-sitter-go` PR 198, opened 2026-08-17,
  unreviewed) adding the missing `type_parameters` field to
  `method_declaration`; when it merges and gotreesitter regenerates its
  Go tables, the gap closes with no change to the index format. The
  Rust bindings share the same grammar file and the same gap.

- **All 206 grammars are embedded.** The full registry costs roughly
  35 MB of binary and no build complexity, and means any repository an
  operator points the service at parses without a rebuild. Build-tag
  subsetting remains available if image size ever matters.
- **Alternatives considered:** the official CGO bindings in a single CGO
  image with static musl linking and an arm64 cross toolchain (new build
  infrastructure no other org repo has, and the one condition under which
  a Rust implementation would have deserved a serious look); a split
  static-plus-CGO image pair (breaks the one-image story); and the
  upstream runtime compiled to WASM under wazero (the two existing Go
  wrappers are stale since early 2025, and upstream has said any WASM
  path would use wasmtime, which is itself CGO).
- **Indexing is rate-limited separately** from review, per tenant, because
  onboarding a large account enqueues many index jobs at once and must not
  starve reviews for everyone else.

### 2.10 Review pipeline

1. **Onboard.** The operator declares the tenant and installation in the
   file. GitHub: the App's settings point at `/hooks/{installation-name}`;
   the installation webhook attaches the installation and enqueues an
   index job per selected repository. GitLab and Forgejo: the operator
   adds the webhook by hand; the service logs the URL and expected header
   at load so there is nothing to guess.
2. **Index** as in 2.9.
3. **PR event** by webhook, or by the periodic poll that is the
   missed-webhook backstop.
4. **Filter gate.** Fork gate, then the resolved CEL expression. Filtered
   PRs enqueue nothing.
5. **Gate on tenant limits.** Concurrency slots always; daily review and
   monthly token caps when set. Deferred jobs snooze; exhausted jobs are
   marked as such and exported as a metric.
6. **Review.** The worker spawns a runner Job; the pod fetches, diffs,
   identifies changed declarations and writes a context pack; the worker
   retrieves chunks from the index, assembles the prompt and calls the
   model with fallback.
7. **Write-back** under the per-PR advisory lock with retry and backoff.
   A review that finds nothing says so.
8. **Follow-up.** A comment that @-mentions the bot re-enters the worker
   scoped to that thread, with the thread and the original findings in
   context.

**konflate integration (optional, per repository).** For a Flux repository
the review worker can fetch konflate's rendered `DiffResult` for the same
PR (blast radius, image changes, danger lint) and include it in the prompt.
It is the `konflate:` key on a repository in the file, and the feature no
commercial peer can offer. Stretch goal for v1; the service must review
non-GitOps repositories with no konflate present.

### 2.11 Model adapters

```go
type Model interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
    Embed(ctx context.Context, inputs []string) ([][]float32, error)
}
```

Two adapters: OpenAI-compatible (base URL plus key, covering OpenRouter,
LiteLLM and local servers) and Anthropic direct. Providers are named in
the file; three roles per tenant, `review`, `indexing`, `fallback`, each a
`provider/model` reference, falling back to `defaults`. Every call records
usage (2.4).

### 2.12 Data model (sketch)

| Entity          | Key fields                                                                                                                                                                                           | Notes                                                                                                              |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `Tenant`        | slug, settings, limits, managed_by                                                                                                                                                                   |                                                                                                                    |
| `Installation`  | tenant id, name, forge kind, host, account, credential kind, secret references, managed_by                                                                                                           | one per bot on one account; hook path is the name                                                                  |
| `Repository`    | tenant id, installation id, forge URI, default branch, settings, enabled, index state, managed_by                                                                                                    |                                                                                                                    |
| `IndexRun`      | repository id, commit SHA, status, model                                                                                                                                                             |                                                                                                                    |
| `Chunk`         | tenant id, repository id, path, byte range, vector                                                                                                                                                   | `pgvector`, HNSW                                                                                                   |
| `PullRequest`   | repository id, number, head SHA, base SHA, author, author is bot                                                                                                                                     |                                                                                                                    |
| `Review`        | pull request id, merge-base SHA, status, model, created at                                                                                                                                           |                                                                                                                    |
| `Finding`       | review id, path, line, severity, body, forge comment id                                                                                                                                              |                                                                                                                    |
| `StickyComment` | pull request id, forge comment id                                                                                                                                                                    | cache; author-plus-marker scan is the source of truth                                                              |
| `Usage`         | tenant id, review id, role, model, input tokens, output tokens                                                                                                                                       | cost visibility per tenant                                                                                         |
| `RunnerRun`     | tenant id, review id or index run id, kind, Job name, pod name, node, phase, created / scheduled / started / finished at, exit code, termination reason, deadline exceeded, log tail, resource usage | one per Kubernetes Job; written by the worker, phase updated by the runner; the only record of a Job after its TTL |

`managed_by` is `file` for every row in v1. v2 adds `dashboard` rows and
the `User`, `TenantMember` and `Session` tables. No secret material in
any table, in either version.

### 2.13 Repository conventions

A sibling of konflate: module `github.com/home-operations/kritik`,
`cmd/kritik/main.go` as wiring only, `internal/` for everything not
exported, `.mise/config.toml` as the single source of tool versions and
tasks, the shared `lefthook.common.toml`, `.golangci.yml` with konflate's
enable list, `release-please` in `simple` mode without a `v` prefix
stamping `charts/kritik/Chart.yaml`, an OCI Helm chart with `helm-schema`
and `helm-docs` generated files and `helm-unittest` suites, a distroless
static image with no Node stage in v1, and konflate's `AGENTS.md` copied
verbatim. The tree-sitter runtime is pure Go (2.9), so the build is the
same `CGO_ENABLED=0` distroless static image as every other Go repo in
the org. The chart gets a `roles` map so a single `all` Deployment and a
split `ingest` plus `worker` topology come from the same values file, and
mounts the config file and referenced Secrets. It also ships the Role and
RoleBinding for Jobs in the release namespace, a runner ServiceAccount
with no permissions, the runner NetworkPolicy, and a `runner` values
block for default resource limits, deadline and TTL, overridable per
tenant in the config file.

**License and hosting.** AGPL-3.0, like every Go project in the org and
like Kodus. home-operations runs a deployment for its own accounts from
the same image and chart; it is not a public service. No dual licensing
and no CLA.

### 2.14 v2: dashboard

Deferred, not dropped. v1 does four things so v2 is additive: Postgres is
the source of truth even though the file is the only writer; usage and
review history are recorded from day one; every runner Job is recorded
as a `RunnerRun` row with its phases, exit status and a bounded log tail,
captured by the worker before the Job's TTL deletes it; and the shared
instance App stays possible through environment variables.

The dashboard therefore learns about runner Jobs only from Postgres. It
never reads Jobs or pod logs through the Kubernetes API: the `web` role
keeps zero cluster permissions, history survives Job garbage collection,
and there is one source for a review's state rather than River rows plus
live Job status to reconcile. Live progress comes from `LISTEN` and
`NOTIFY` on the `RunnerRun` and job rows, which the runner updates as it
moves through clone, parse and write phases using the scoped database
role it already holds.

v2 adds, in one release:

- A `web` role serving a UI on **the same stack as konflate, nothing
  added**: Svelte 5 with runes, Vite, Tailwind, TypeScript, no component
  library; Geist and Geist Mono via `@fontsource-variable`, icons from
  `@mdi/js` and `simple-icons`; Playwright for end-to-end tests; built
  into `internal/web/dist` and embedded with `go:embed`, with the same
  `node` build stage in the Dockerfile and the same `ui-*` mise tasks.
  Pages: tenants, installations, repository list with index state, PR
  list with last outcome, review detail with findings, the follow-up
  thread and the runner timeline (phases, exit status, log tail), usage,
  and a queue view of in-flight and deferred jobs.
- **Sign-in by forge OAuth plus generic OIDC**, either or both per
  deployment. GitHub, GitLab or a Forgejo instance as OAuth providers,
  doubling as the tenant-membership check against declared accounts (for
  a personal-account tenant, being that account). Any OIDC issuer via
  discovery URL, client id and secret, with membership by invitation when
  the issuer carries no forge identity. Signed session cookies backed by a
  session table.
- **Dashboard-managed objects.** Tenants, installations and repositories
  created in the UI carry `managed_by = dashboard`; file-managed rows are
  read-only there, and a slug collision between the two is an error, not a
  merge. Credentials entered in the UI are envelope-encrypted with a key
  from the environment; file-managed credentials stay references.
- Roles: instance operator (environment-configured allowlist of subjects),
  tenant admin, tenant member. All state-changing endpoints require a
  tenant admin. Every write is audit-logged.

Whether the dashboard should ever be able to edit what the file manages
is a v2 question; the default answer is no, so that git stays the source
of truth for anything declared in git.

---

## 3. Consequences

- **v1 is konflate-shaped.** Environment plus a file, no auth, a read-only
  HTTP surface that exposes only `/hooks` and health, one binary, one
  Postgres. It is a shape the org already knows how to build, test and
  ship, and the Node toolchain, OIDC, sessions and RBAC are absent from
  the first release.
- **konflate is untouched.** Its forge, git, webhook and filter packages
  are copied and adapted here. The cost is two copies of the GitHub App
  transport and the sticky-comment logic to keep in sync by hand.
- **Kubernetes and Postgres with `pgvector` are hard requirements.** The
  homelab story is a cluster with a CloudNativePG database on a
  `standard` image tag; a `minimal` image will not work, and there is no
  compose or VM path. This trades konflate's run-anywhere quick start for
  a pod-per-job trust boundary, which is the right trade for a tool that
  checks out other people's pull requests.
- **Review latency includes pod start.** Scheduling and image pull add
  seconds to a job measured in minutes. Clusters that scale nodes on
  demand pay more; a small always-on node pool for runners is the fix if
  it shows.
- **Review history for a private repository is visible only in the PR and
  the database** until v2. That is the correct default rather than a
  loss.
- **The `ai-review` action stays** until the service reviews the org's
  own repositories at Robin's quality or better. The pilot caller in
  `flate` is the first repository to point the service at.
- **Every tenant is operator-trusted**, which keeps the public-service
  abuse surface out of scope. What remains is what any forge bot faces:
  fork PRs putting untrusted text in front of the model and reaching
  untrusted remotes, and cost from large monorepos. The runner pod
  boundary and its NetworkPolicy cover the first; usage metrics and
  optional caps cover the second.

---

## 4. Open questions

- **Embedding model and chunk metadata.** Which embedding model the
  `indexing` role defaults to, and how much of the tree-sitter metadata
  (signature, enclosing scope, imports) is embedded with the chunk versus
  stored alongside it for the prompt. Follow-up ADR, together with the
  prompt design.
- **Go 1.27 generic methods in `tree-sitter-go`.** Ship without it.
  Watch upstream PR 198 and re-run the parity check when a grammar
  release lands. If it drags on, gotreesitter's grammar authoring path
  can extract tables from the PR branch's `parser.c` as a build step.
