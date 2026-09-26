# ADR-0010: configuration layers: environment, file, dashboard and repository

- **Status:** Proposed
- **Date:** 2026-09-26
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.6 (the two
  configuration sources), [ADR-0003](0003-forgejo-agentic-review.md) §2.3
  (`.kritik.yaml`) and [ADR-0009](0009-web-dashboard.md) §2.12 (collisions),
  §2.15 (operator-only fields) and the first item of §6.
- **Authors:** onedr0p.

> Scope: where each setting lives, who may change it, which value wins when
> more than one layer speaks to the same repository, how the dashboard shows
> what it may not change, and what a repository's own `.kritik.yaml` may
> choose. It does not change how a review, follow-up or index run works once
> its settings are resolved.

## 1. Context

Configuration reaches kritik from four places today, and each grew on its
own:

- **Environment variables** (`internal/config/config.go`): listeners, DSNs,
  keys, the embedder, the executor and runner image, but also tuning such as
  `KRITIK_RUNNER_DEADLINE`, `KRITIK_POLL_INTERVAL`, `KRITIK_POLL_LOOKBACK` and
  `KRITIK_ONBOARD_WINDOW`. `KRITIK_RUNNER_DEADLINE` is in fact the default of
  a per-tenant file field (`runner.activeDeadlineSeconds`), so one setting
  spans two layers.
- **The config file** (`internal/configfile`): providers, `defaults`, egress,
  retention, `web`, and file-managed tenants with their installations and
  repositories, resolved defaults → tenant → repository by `File.Settings`.
- **Postgres** (`dashboard_tenants`): dashboard-managed tenant specs,
  merged into the file's snapshot (ADR-0009 §2.5). File and dashboard tenants
  are disjoint by slug; a file tenant is read-only in the UI as a whole.
- **`.kritik.yaml`** (`internal/repoconfig`, ADR-0003 §2.3): read by the
  runner from the merge base and merged onto the operator's settings.

Surveying the code found these problems:

- **No stated precedence.** A repository's `filter` replaces its tenant's,
  but `.kritik.yaml`'s `filter` is ANDed with the operator's. A zero or empty
  value means "inherit" (`resolve.go`), so a tenant cannot clear a default
  filter, settle or limit.
- **`.kritik.yaml` does not only narrow**, despite ADR-0003 §2.3 and
  `docs/repository-config.md`: its `instructions` replace the operator's list,
  and `requireSuggestedFix: false` overrides an operator's `true`. It is also
  ignored by indexing and follow-ups, and it cannot pick a mode, model or
  agent limit at all.
- **Two different operator-only lists.** Fields closed to `.kritik.yaml`
  (`mode`, `agent`, `incremental`, `settle`) differ from fields closed to a
  tenant admin (`models`, `forks`, `runner`, `limits`, and a repository's
  `agent`, `mode`, `incremental`), and the second list is repeated in
  `internal/webapi/policy.go`, the dashboard's form, `docs/dashboard.md` and
  ADR-0009 §2.15.
- **Uneven levels.** `mode`, `agent`, `incremental` and `review` exist only
  per repository; `models`, `limits` and `forks` have no repository level;
  `runner` exists only per tenant, with its default in the environment.
- **"Postgres is the source of truth" (ADR-0002 §2.6) is not what the code
  does.** Every component reads the merged in-memory snapshot
  (`configfile.Current`); the `tenants.settings` and `repositories.settings`
  columns are written and never read.
- **A file edit that collides with a dashboard tenant blocks the whole
  file**, not just the colliding tenant: the last good snapshot stays live for
  every tenant.

## 2. Decision

### 2.1 Four layers, one owner per setting

| Layer                | Written by                                       | Holds                                                        | Changes take effect                    |
| -------------------- | ------------------------------------------------ | ------------------------------------------------------------ | -------------------------------------- |
| Environment          | whoever deploys the process                      | deployment wiring and secrets (§2.2)                         | on restart                             |
| Config file          | the operator, through git                        | instance settings, and every tenant it declares (§2.3)       | on reload, applied by the leader       |
| Dashboard (Postgres) | operators and tenant admins, through the UI      | every tenant the file does not declare (§2.3)                | on write, via `NOTIFY`                 |
| `.kritik.yaml`       | whoever can push to the repository's base branch | the repository's choices within the operator's bounds (§2.5) | per run, from the commit the run reads |

**Every setting, at a given scope, has exactly one layer that may set it.**
Precedence (§2.4) decides how defaults flow down from a broader scope and how
far a repository may move a value inside its bounds. It never decides a
contest between two writers of the same field at the same scope, because no
such contest is allowed to exist.

There is **one read path**: every component reads the merged, validated
snapshot (`configfile.Current`). Postgres stores the dashboard-owned specs,
memberships and runtime state; it is not a second copy of the configuration.
The write-only `tenants.settings` and `repositories.settings` columns are
dropped, and so is the parsed-but-unread repository `konflate` field.

### 2.2 The environment holds wiring and secrets only

A value belongs in the environment when the process needs it before it can
read the file or the database, when it differs per process or replica, when
it describes the deployment's own infrastructure (listeners, cluster
objects, images), or when it is a secret. Everything that changes how
reviews, follow-ups or indexing behave is configuration and lives in the
file.

**Stays in the environment:**

| Group                                                                                  | Variables                                                                                                                                                                                                             |
| -------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Listeners and process                                                                  | `KRITIK_ADDR`, `KRITIK_METRICS_ADDR`, `KRITIK_GATEWAY_ADDR`, `KRITIK_WEB_ADDR`, `KRITIK_WEB_URL`, `KRITIK_LEADER_RETRY_INTERVAL`, `KRITIK_LOG_LEVEL`, `KRITIK_LOG_FORMAT`                                             |
| Configuration source                                                                   | `KRITIK_CONFIG_FILE`, `KRITIK_CONFIG_RELOAD_INTERVAL`                                                                                                                                                                 |
| Database                                                                               | `KRITIK_DATABASE_URL`, `KRITIK_DATABASE_OWNER_URL`, `KRITIK_DATABASE_APP_ROLE`, `KRITIK_DATABASE_RUNNER_ROLE`                                                                                                         |
| Keys                                                                                   | `KRITIK_DASHBOARD_KEY`, `KRITIK_DASHBOARD_OLD_KEYS`                                                                                                                                                                   |
| Embedding, deployment-wide because it shapes the `index_chunks` schema (ADR-0002 §2.6) | `KRITIK_EMBED_BASE_URL`, `KRITIK_EMBED_API_KEY`, `KRITIK_EMBED_MODEL`, `KRITIK_EMBED_DIMS`, `KRITIK_EMBED_MAX_BATCH`, `KRITIK_EMBED_MAX_BATCH_CHARS`, `KRITIK_EMBED_MAX_ITEM_CHARS`, `KRITIK_REINDEX_ON_MODEL_CHANGE` |
| Gateway                                                                                | `KRITIK_GATEWAY_URL`, `KRITIK_GATEWAY_TOKEN_TTL` (a credential's lifetime)                                                                                                                                            |
| Executor and runner pods                                                               | `KRITIK_EXECUTOR`, `KRITIK_RUNNER_IMAGE`, `KRITIK_RUNNER_SERVICE_ACCOUNT`, `KRITIK_RUNNER_DATABASE_SECRET`, `KRITIK_RUNNER_DATABASE_SECRET_KEY`, `KRITIK_RUNNER_RUNTIME_CLASS`, `KRITIK_RUNNER_TTL`                   |
| Per-process capacity                                                                   | `KRITIK_REVIEW_WORKERS`, `KRITIK_INDEX_WORKERS`                                                                                                                                                                       |
| Runner-only, injected per run                                                          | `KRITIK_RUN_SPEC_FILE`, `KRITIK_GIT_TOKEN`, `KRITIK_GATEWAY_TOKEN`, `KRITIK_RUNNER_DATABASE_URL` (local executor)                                                                                                     |

**Moves to the file:**

| Variable                                       | Becomes                                 | Why                                                                                                      |
| ---------------------------------------------- | --------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `KRITIK_RUNNER_DEADLINE`                       | `defaults.runner.activeDeadlineSeconds` | it is already the default of the tenant's `runner.activeDeadlineSeconds`; the chain belongs in one layer |
| `KRITIK_POLL_INTERVAL`, `KRITIK_POLL_LOOKBACK` | `polling.interval`, `polling.lookback`  | how soon a missed webhook is caught is review behaviour                                                  |
| `KRITIK_ONBOARD_WINDOW`                        | `indexing.onboardWindow`                | how fast repositories are onboarded is indexing behaviour                                                |

The chart renders these into the file instead of the environment. kritik has
no release yet, so the old variable names are removed rather than aliased.

No setting may be both an environment variable and a file key, so neither
layer ever overrides the other. A secret reference `{ env: NAME }` in the
file is a reference, not precedence: the file names the variable, and the
environment only supplies its value.

### 2.3 The config file owns whole scopes

**The instance scope is the file's alone:** `providers`, `defaults`,
`egress`, `retention`, `web`, `polling` and `indexing`. The dashboard shows
them read-only to operators (§2.7) and never writes them. A deployment
therefore always has a file; sign-in and operators live in it
(ADR-0009 §2.3), so no dashboard session can widen who operates the
instance.

**A tenant has exactly one owner: the file or the dashboard.** The owner
holds the whole tenant: its settings, its installations and their
credentials, and its repositories and their settings. There is no per-field
overlay: the dashboard never tunes, shadows or partly overrides a tenant the
file declares. This settles ADR-0009 §6's first deferred question, whether
the dashboard may edit what the file manages: it may not. Anything declared
in git stays described entirely by git.

**Membership is not configuration.** Members and invites live in Postgres
for every tenant, whoever owns it, as today; granting access does not change
how reviews behave.

**Collisions.** A dashboard write that would take a slug or installation
name the file already holds is refused with `409 slug_taken`, as ADR-0009
§2.12 decides. A file edit that claims a slug or installation name a
dashboard tenant already holds now wins instead of blocking the whole
reload: the dashboard tenant is disabled, kept rather than deleted, and
marked as conflicting; the operator console, `kritik_config_error` and the
audit log say so, and every other tenant in the file applies. The file is
the higher authority, and one conflict must not freeze every tenant's
configuration at the last good snapshot.

### 2.4 Precedence

Two rules decide every value:

1. **Within one author, the narrower scope replaces the broader one.** The
   operator, whether through the file or through a dashboard tenant, sets a
   value at the built-in default, `defaults`, tenant or repository scope, and
   the narrowest scope that sets it wins. Presence decides, not zero: a field
   written at a narrower scope replaces the inherited value even when it is
   empty or zero, so a tenant can clear a default filter, settle or limit. A
   field left out inherits.
2. **Across authors, the less trusted one narrows or chooses within bounds.**
   `.kritik.yaml` is written by the repository, not the operator. It is
   applied last, and it can never move a value outside the bounds the
   operator's resolved settings allow (§2.5).

Every repository-scoped setting is available at all three operator scopes
(`defaults`, tenant, repository), and every tenant-scoped setting at
`defaults` and tenant:

| Setting                                                                   | Operator scopes                                  | `.kritik.yaml`                                      |
| ------------------------------------------------------------------------- | ------------------------------------------------ | --------------------------------------------------- |
| `enabled`                                                                 | repository                                       | may turn off only                                   |
| `filter`                                                                  | defaults, tenant, repository                     | ANDed with the operator's                           |
| `ignore`                                                                  | built-in, defaults, tenant, repository (unioned) | unioned; cannot remove an operator glob             |
| `forks`                                                                   | defaults, tenant, repository                     | none                                                |
| `models` (`review`, `fallback`)                                           | defaults, tenant, repository                     | may pick from `allow.models`                        |
| `mode`                                                                    | defaults, tenant, repository                     | may pick from `allow.modes`                         |
| `agent` limits (`maxSteps`, `maxToolOutputBytes`, `maxTokens`, `timeout`) | defaults, tenant, repository                     | may set each at or below `allow.agent`              |
| `agent.commands`                                                          | defaults, tenant, repository                     | may pick a subset of `allow.commands`               |
| `agent.commandTimeout`                                                    | defaults, tenant, repository                     | none                                                |
| `settle`                                                                  | defaults, tenant, repository                     | may set at or below `allow.settle`                  |
| `incremental.maxDeltaFiles`                                               | defaults, tenant, repository                     | none                                                |
| `review.instructions`                                                     | defaults, tenant, repository                     | appended after the operator's                       |
| `review.requireSuggestedFix`                                              | defaults, tenant, repository                     | may turn on only                                    |
| `review.templates`                                                        | defaults, tenant, repository                     | may replace (presentation grants nothing, ADR-0005) |
| `limits`, `runner`                                                        | defaults, tenant                                 | none                                                |
| instance settings (§2.3)                                                  | file only                                        | none                                                |

The environment does not appear in this table: nothing it holds is a
per-repository setting (§2.2).

### 2.5 `.kritik.yaml`: choices within the operator's bounds

The file lives at the root of the repository as `.kritik.yaml`. It is read
from the merge base for a review or follow-up, and from the indexed commit
for an index run: base-branch history, which the pull request under review
cannot rewrite (ADR-0003 §2.3).

**It holds nothing secret.** It has no field that takes a credential, a
URL, a host, a provider definition or an egress entry, and its decoder,
which is already strict, also refuses every secret-reference form (`env`,
`file`, `sealed`). It can only name what the operator already configured: a
model by its `<provider>/<model>` reference, a command by its name.

**The operator grants choice with an `allow` block**, at `defaults`, tenant
or repository scope, resolved like any other setting (§2.4):

```yaml
defaults:
  mode: single
  allow:
    modes: [single, agentic]
    models: [openrouter/openai/gpt-6-mini, openrouter/openai/gpt-6-sol]
    commands: [rg, fd]
    agent: { maxSteps: 60, maxTokens: 8000000, timeout: 20m }
    settle: 30m
```

With no `allow`, a repository may only narrow: pick a value at or below the
operator's own, as today. Load rejects an operator value outside its own
`allow`. The repository then chooses:

```yaml
mode: agentic
models: { review: openrouter/openai/gpt-6-mini }
agent: { maxSteps: 40, commands: [rg] }
settle: 5m
filter: '!pr.body.contains("[skip-review]")'
ignore: ["web/src/generated/**"]
skip: { onlyPaths: ["docs/**"] }
review:
  instructions: [".kritik/rules.md"]
  requireSuggestedFix: true
```

**A value outside its bounds is dropped, not clamped**: the operator's value
applies for that field, a note in the sticky comment says which field was
dropped and why, and the rest of the file still applies. Clamping would
silently run the repository with a value nobody wrote. A file that does not
parse is ignored whole and noted, as today.

**Two current behaviours change to match the table.** `review.instructions`
are appended after the operator's instead of replacing them, since the
operator's instructions are policy. `requireSuggestedFix` may only turn on,
since strictness grants nothing but its absence would loosen the operator's.

**It now also applies to follow-ups and indexing.** `enabled: false` stops
reviews, follow-ups and indexing; `ignore` applies to indexing; a follow-up
uses the repository's `models` and `instructions`.

**Never chosen by a repository:** `limits`, `forks`, `runner`,
`incremental`, `agent.commandTimeout`, and every instance setting. These size
what the operator pays for or expose the instance to untrusted code, and no
`allow` block can open them.

### 2.6 Reading `.kritik.yaml` before the run

A repository's `mode` and `models` decide which model slot the worker waits
for and what run spec it builds, and its `settle` decides when the job
starts. All three are decided before the runner exists, but the runner is
what reads `.kritik.yaml` today. So:

- **The worker reads `.kritik.yaml` first.** A new `forge.Client.FileAt(ctx,
owner, repo, ref, path)` fetches the one file at the merge base (or, for an
  index run, at the indexed commit) through the forge API: the same commit,
  so the same trust root, as the runner's tree. The worker validates it,
  applies the bounds, and puts the effective settings in the run spec. The
  runner still reads the files `.kritik.yaml` names (instructions,
  templates) from the merge-base tree under the existing size caps, but no
  longer merges policy.
- **Settle moves from ingest to the worker.** Ingest enqueues the review job
  at once; the worker reads the effective `settle` and snoozes the job until
  that long after the head arrived. A job that wakes to find the pull
  request's head has moved on ends as `superseded`, as a stale job does
  today.
- **Cost:** one forge API call per review, follow-up and index job.

### 2.7 Provenance in the API, and what the UI disables

Every configuration view the API serves (instance, tenant and repository)
returns, for each field, its **value**, its **source** (`env`, `file`,
`dashboard`, `repository` for `.kritik.yaml`, or `default`), and whether the
**caller may edit it**. A secret's value is never returned from any layer;
the API reports only whether it is set (ADR-0009 §2.13).

Editability is computed on the server from **one policy table in code**: for
each field and scope, which authors may set it (file, operator, tenant admin,
repository) and within which bounds. It replaces the duplicated lists in
`internal/webapi/policy.go`, the dashboard's form, `docs/dashboard.md`,
ADR-0003 §2.3 and ADR-0009 §2.15, and the repository merge reads the same
table.

The UI renders from that response, never from its own list:

- A field the caller may not edit is shown **disabled (greyed out)** with its
  source; a tenant the file owns renders its whole form disabled, under a
  note that the config file manages it.
- A secret is always **masked**, showing only whether it is set; it is also
  greyed out when the file or the environment supplies it, and otherwise is
  write-only as ADR-0009 §2.13 decides.
- The operator console shows the instance settings read-only, each with its
  source: `env`, `file` or `default`.
- A repository's view shows the values its `.kritik.yaml` chose, read-only,
  with the commit they came from and next to the operator's value and
  bound, plus any field dropped for being out of bounds.

The write API enforces the same table and never relies on the UI having
disabled a field.

## 3. Consequences

- **Operators can let repositories opt in to more** (agentic mode, a
  different model, more steps) without editing kritik's configuration for
  each repository, and a repository cannot exceed what the operator allowed.
  The default, no `allow` block, keeps today's narrow-only behaviour.
- **Anyone who can push to a repository's base branch** can now raise that
  repository's cost up to the operator's bounds. That is the same audience
  that already controls the review instructions, and the bounds are what the
  operator chose to spend.
- **GitOps stays exact:** a tenant declared in the file is fully described by
  the file, and the dashboard can only display it.
- **Changing the poll, onboarding window or default runner deadline** is a
  config reload instead of a restart.
- **One forge API call more** per review, follow-up and index job, and a
  `FileAt` implementation on each forge client.
- **Breaking changes, acceptable before the first release:** the three moved
  environment variables are removed, and the chart's `config.pollInterval`,
  `config.pollLookback`, `config.onboardWindow` and runner deadline values
  render into the file. `.kritik.yaml` `instructions` now append, and a
  `requireSuggestedFix: false` no longer loosens the operator's `true`.
- **Documentation:** a configuration reference covering every layer and the
  precedence table, and a corrected `docs/repository-config.md`.
- A command named in `allow.commands` or `agent.commands` must still be on
  the runner image's `PATH` (ADR-0008 §2.2); the image stays deployment
  wiring (§2.2), and a missing command is still not offered to the agent.

## 4. Rejected alternatives

- **The dashboard tuning a file tenant field by field**, where anything the
  file leaves unset is editable in the UI. The file would then no longer
  describe the tenant's running configuration, and a field's owner could
  change just by adding it to or removing it from the file.
- **The dashboard overriding the file.** Git would stop being the source of
  truth for what it declares.
- **The dashboard managing instance settings, with the file optional.** It
  would need a bootstrap path for the first operator and would put sign-in
  and operators within reach of a dashboard session.
- **`.kritik.yaml` overriding the operator's tuning.** A repository could
  raise cost and exposure past anything the operator sized the instance for.
- **Clamping an out-of-bounds value** (§2.5). It runs a value nobody wrote.

## 5. Deferred

- Per-tenant providers, so a dashboard tenant can bring its own model key
  (ADR-0003 §2.6 recommends a key per tenant; today every tenant uses the
  file's providers).
- Picking up a rotated `file:` secret without a change to the config file's
  own bytes.
- Path-scoped rules in `.kritik.yaml` (settings that apply only under some
  paths).
