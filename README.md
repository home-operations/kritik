<div align="center">

# kritik

**Repository-aware AI pull request review for GitHub, GitLab and Forgejo.**

[![CI](https://img.shields.io/github/actions/workflow/status/home-operations/kritik/ci.yaml?branch=main&label=ci)](https://github.com/home-operations/kritik/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/actions/workflow/status/home-operations/kritik/release.yaml?branch=main&label=release)](https://github.com/home-operations/kritik/actions/workflows/release.yaml)
[![License](https://img.shields.io/github/license/home-operations/kritik)](https://github.com/home-operations/kritik/blob/main/LICENSE)

</div>

kritik indexes a repository, reviews each pull request against that context,
posts one sticky summary comment plus inline findings, and answers follow-ups
when the bot is @-mentioned. One image serves any number of forge accounts
from one deployment; every index and review job runs in its own Kubernetes
Job pod that holds no secrets.

The design is recorded in
[`docs/adr/0002-kritik-pr-review-service.md`](docs/adr/0002-kritik-pr-review-service.md),
amended by [`docs/adr/0003-forgejo-agentic-review.md`](docs/adr/0003-forgejo-agentic-review.md)
(Forgejo, providers, the contract, `.kritik.yaml`, agentic mode) and
[`docs/adr/0004-model-gateway.md`](docs/adr/0004-model-gateway.md) (the worker
as model gateway, no provider key in a runner pod) and
[`docs/adr/0005-go-templates.md`](docs/adr/0005-go-templates.md) (comment
templates are Go templates with sprout) and
[`docs/adr/0006-finding-fixes.md`](docs/adr/0006-finding-fixes.md) (findings
carry a replacement the forge offers as a suggestion, and an agent prompt),
[`docs/adr/0007-vectorchord.md`](docs/adr/0007-vectorchord.md) (the vector
index is VectorChord) and
[`docs/adr/0008-runner-tools.md`](docs/adr/0008-runner-tools.md) (the agent
may run allowlisted commands; runner egress goes through the gateway) and
[`docs/adr/0009-web-dashboard.md`](docs/adr/0009-web-dashboard.md) (the web
dashboard: sign-in, dashboard-managed tenants, live updates and the full
model conversation); ADR-0001 is kept as historical input.
What works today: the configuration file loader with live reload, the
Postgres store with row-level security and River, the ingest role (a signed
forge webhook becomes rows and a review job), and the worker role, which
takes the merge-base from the forge, runs a Kubernetes Job per review
that fetches the two commits, diffs them and writes a context pack, then
holds a per-tenant model lease, asks a configured model for structured
findings, and writes back a sticky summary comment, inline review comments
and a commit status. A finding whose fix is a change to the lines it points
at carries the replacement, which the inline comment offers as a one-click
suggestion, and a prompt a coding agent can apply the fix from. GitHub and Forgejo are both wired forges; a
`providers` entry picks the `openrouter`, `openai` or `anthropic` adapter
per tenant, with per-model `pricing` to cost calls a provider itself
doesn't report a cost for. A repository's `mode` can ask for an agentic
review instead of the default single pass: a bounded, read-only tool loop
(listing, reading and grepping the repository, capped steps and tool
output, a wall-clock timeout) that lets the model pull more of the
repository into its own context before submitting structured findings.
Its `agent.commands` can add a `run` tool: the model runs an allowlisted
binary (`curl`, `fd` and `rg` ship in the `-tools` image) with its own
arguments, without a shell, over a checkout of the head commit, so it can
read a dependency bump's release notes and compare view; the sticky
comment lists every URL it fetched. The agent reaches its model through
the worker's gateway with a token good for its run alone: the provider key
never enters the runner pod, and the gateway checks the run's token budget
and the tenant's monthly cap before every step and records its usage.
`settle` delays a new head's
review so a burst of force-pushes only costs one; a later push builds on
the pull request's last completed review, scoped to what changed since,
as long as that review's head is still reachable and fewer files changed
than a configurable ceiling, and falls back to a full review otherwise. A
repository's own `.kritik.yaml`, read
from the merge-base tree so a pull request can't use it to weaken its own
review, can narrow what the operator allows (disable itself, add an
additional filter or ignore globs, skip a pull request whose changes match
only certain paths) and set its own instructions and comment templates;
see [`.kritik.yaml` reference](#kritikyaml-reference) below. The runner
also builds the context the prompt carries beyond the diff: whole
declarations the diff touches, definitions of identifiers on changed
lines, and callers of changed declarations, all cut by tree-sitter (pure
Go, every grammar embedded; the stripped static binary is about 122 MiB).
With a deployment embedder configured (`KRITIK_EMBED_*`), the leader
onboards every declared repository into a VectorChord index of the default
branch, default-branch pushes advance it incrementally, and reviews add
the most similar indexed chunks as a fourth context stage. An @-mention of
the bot by someone with write access gets an answer in the thread, with
the diff, the review's context and findings, and the thread in the prompt,
five per pull request per hour. The leader also polls each installation's
open pull requests on an interval as a backstop for missed webhooks; a
head the webhook already enqueued is deduplicated by the job queue. The
web role serves a dashboard: sign-in, dashboard-managed tenants and their
members, live review and conversation state, and a per-tenant and
operator audit log; see [Dashboard](#dashboard) below. GitLab support,
the Helm chart's remaining hardening and the evaluation harness come
next; see the ADR for the rest.

## Installing

kritik ships as an OCI Helm chart, `oci://ghcr.io/home-operations/charts/kritik`.
The chart's [README](charts/kritik/README.md) lists every value and shows the
CloudNativePG setup for the three database roles. In short: a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) (and the pgvector
it builds on) loaded, with an owner, an application and a runner role, the configuration
file under `config.file`, the secrets it references under `secretMounts`, and
optionally an embedder under `embedding` for the index. `roles.all` runs the
single-process topology; `roles.ingest` and `roles.worker` split it.

Four security notes. Install kritik into a namespace of its own: runner Jobs
run in the release namespace, and the worker's Role can create, patch and
delete every Secret there, though it can never get or list one. Keep the
egress gateway on (the chart's default) with a NetworkPolicy: runner pods then
reach the outside only through the worker's forward proxy, which allows
destinations by hostname (the forges and the file's `egress.allowHosts`),
and never hold the credentials `egress.credentials` lets the gateway add;
agentic reviews need it, since it is also where their model calls go. Run runner Jobs under a sandboxed RuntimeClass such as
gVisor (`runner.runtimeClassName`) where the cluster has one, since the pod
parses untrusted content; it is advised, not required. And give a Forgejo
installation a read-only `gitToken` beside its `token`: runners fetch with
`gitToken` when it is set, and otherwise with `token`, which can write to
the forge, inside the pod that reads untrusted pull request content.

## `.kritik.yaml` reference

A repository may commit an optional `.kritik.yaml` at its root to narrow how
kritik reviews it. It is read from the merge-base commit, never the pull
request's own tree, so a pull request cannot use its own copy to weaken the
review applied to it; a file that fails to parse is ignored as a whole, and
noted rather than failing the review. It can only narrow what the operator
already allows — `mode`, `agent`, `incremental` and `settle` stay
operator-only — and its keys are:

- `enabled: false` — disables review for the repository (it cannot turn a
  disabled repository back on).
- `filter` — a filter expression ANDed with the operator's own; it is
  compiled and smoke-tested against a sample pull request when the file is
  parsed, so a broken expression is rejected rather than silently skipping
  every review.
- `ignore` — path globs added to the operator's own ignore list.
- `skip.onlyPaths` — path globs; the pull request is skipped only when
  every changed path matches at least one of them.
- `review.instructions` — paths to files (read from the same merge-base
  tree) appended to the reviewer's system prompt, capped at 32 KiB joined.
- `review.requireSuggestedFix` — whether findings must include a suggested
  fix.
- `review.templates.summary` / `review.templates.inline` — paths to Go
  [text/template](https://pkg.go.dev/text/template) templates that replace
  kritik's built-in summary and inline comment templates, with the
  [sprout](https://github.com/go-sprout/sprout) helpers tuppr and chaski
  expose (std, strings, conversion, encoding, numeric, slices, maps, regex,
  time, semver and reflect; not env, filesystem, network, random, uniqueid
  or checksum, and not `set` or `unset`). The `template`, `define` and
  `block` actions are refused, so a template cannot read any file or call
  any other template. The summary template's dot is the review (`.Number`,
  `.HeadSHA`, `.Model`, `.Result.Summary.Take`, `.Result.Summary.Praise`,
  `.Result.Findings`, `.Counts.Blocking`/`.Important`/`.Nit`, `.Notes`,
  `.Incremental`, `.PriorHeadSHA`, `.Incomplete`); the inline template's dot
  is one finding (`.Path`, `.Line`, `.EndLine`, `.Severity`, `.Title`,
  `.Explanation`, `.SuggestedFix`, `.Replacement`, `.AgentPrompt`, `.URL`, a
  link to the lines at the head commit). Rendering is bounded (loop iterations, bytes per
  function call, output size, a deadline) so a template cannot hang or
  exhaust memory; one that exceeds a bound falls back to the default with a
  note in the comment.

Every referenced file, plus `.kritik.yaml` itself, is capped at 256 KiB,
and 1 MiB in total; a file over either limit is skipped and noted rather
than failing the review.

## Dashboard

The web role serves a dashboard: sign in with GitHub, Forgejo or any OIDC
provider, and see the tenants you belong to, their installations and
repositories, live review and conversation state as it runs, member and
invite management, and a per-tenant and (for operators) instance-wide audit
log. A tenant admin can also queue a re-run of a specific pull request,
cancel a review in progress, or reindex a repository's embeddings, from the
dashboard rather than the forge.

Enable it with role `web` (a dedicated listener) or `all` (which also serves
it once `KRITIK_WEB_URL` is set, alongside the other roles); `KRITIK_WEB_ADDR`
is where it listens (default `:8083`), and `KRITIK_WEB_URL` is its
externally reachable origin — an absolute `http(s)` URL with no query or
fragment, required for the web role, used to build sign-in callback URLs and
the session cookie's scope. In the chart, `roles.web.enabled` turns the role
on, `web.url` sets `KRITIK_WEB_URL`, and `web.port` matches `KRITIK_WEB_ADDR`'s
port (8083 by default); `ingress.web` and `httpRoute.web` are the Ingress and
Gateway API HTTPRoute for it, and `dashboard.keySecret` names the Secret
holding the sealing key (below).

### `web:` configuration reference

The `web:` block, a sibling of `tenants:` at the file's root, controls who
may sign in and who of them may operate the instance:

- `signIn` — one entry per identity provider, each with a `name` (used in
  the callback URL and in `operators`), a `type` of `oidc`, `github` or
  `forgejo`, a `clientId`, and a `clientSecret` (a secret reference: `env`,
  `file` or `sealed`). Every provider must allow the callback URL
  `<KRITIK_WEB_URL>/auth/callback/<name>`. For example:

  ```yaml
  web:
    signIn:
      - name: sso
        type: oidc
        issuer: https://idp.example.com
        clientId: kritik-dashboard
        clientSecret: { env: OIDC_CLIENT_SECRET }
        scopes: [openid, email, profile]
      - name: github
        type: github
        clientId: Iv1.abc123
        clientSecret: { env: GITHUB_CLIENT_SECRET }
      - name: ghe
        type: github
        host: github.example.com
        clientId: abc123
        clientSecret: { file: /run/secrets/ghe-client-secret }
      - name: forgejo
        type: forgejo
        host: forgejo.example.com
        clientId: abc123
        clientSecret: { env: FORGEJO_CLIENT_SECRET }
  ```

  `oidc` takes `issuer` (an `https` URL) and no `host`; `github` and
  `forgejo` take `host` and no `issuer`. A `github` sign-in with no `host`
  is `github.com` — a GitHub Enterprise instance is still `type: github`,
  just naming its own `host`; `forgejo`'s `host` is always required.

- `operators` — the identities allowed to change configuration, each
  `"<signIn name>:<login or subject>"` (the forge login or OIDC subject) or
  `"email:<address>"`, matched against the address a sign-in reports.
  `email` is reserved and cannot name a `signIn`. An operator creates and
  deletes dashboard tenants from the operator console and is the only one
  who may set the operator-only fields below.
- `sessionTTL` — how long a dashboard session lasts, between 5 minutes and
  30 days; defaults to 12 hours.
- `dashboardForgeHosts` — the forge hosts a dashboard-managed tenant's
  installations may use. Every installation host is also an allowed runner
  egress host, so this bounds what a tenant admin, who did not write the
  operator's file, can point kritik at; empty means `github.com` plus
  whatever hosts the file's own installations already use. No wildcards.

### Roles

Three roles share the same `web.signIn` and `web.operators`:

- **Operator** — an identity in `web.operators`. The only one who can edit
  the configuration file, the only way a dashboard tenant is created, and
  the only one who may set a dashboard tenant's `models`, `forks`,
  `runner`, `limits`, `repositories[].agent`, `repositories[].mode` or
  `repositories[].incremental`; a tenant admin's write that touches any of
  those is rejected. Membership is checked per source (the forge, refreshed
  at sign-in, and accepted invites), and a principal who qualifies through
  more than one gets the highest of the roles it grants.
- **Tenant admin** — can edit a dashboard-managed tenant's configuration,
  installations and repositories (other than the operator-only fields
  above), invite and remove members, and queue a re-run, cancel or
  reindex; every one of those writes is audit-logged in the same
  transaction as the change it makes. A dashboard installation may only
  reach its forge over `https`; a plain-`http` host is refused. A secret an
  admin submits (a client secret, an installation token) is bound to that
  installation's forge, host (scheme and path included) and account —
  change any of them and the secret must be re-entered, since it no longer
  speaks for the same identity. The form never keeps a renamed
  installation's secrets. In the advanced JSON editor, as through the API,
  `{"keep": true}` keeps the secret stored under the name the JSON gives:
  renaming an installation there does not carry its secrets along (the
  keep is refused, or takes the secret of a stored installation that
  already had the new name, when its forge, host and account match), so
  enter them again when renaming in JSON.
- **Tenant member** — read access to their tenant's own reviews,
  conversations and transcripts; no write access.

Re-run, cancel and reindex all respond `202 Accepted` (with a job ID for
re-run and reindex) and queue the work rather than running it inline;
re-running a pull request with no known head, or cancelling a review that
is not running, is a `409 Conflict`. Inviting an address that is already a
member of the tenant is refused, `409 already_member`, instead of creating
a duplicate, and claiming a slug another tenant already holds, file- or
dashboard-managed, is `409 slug_taken`. Deleting a dashboard tenant removes
its memberships and invites along with it, but not its history: its
reviews, findings, usage and transcripts stay keyed on its slug. Creating a
tenant under a slug any tenant held before is therefore also `409
slug_taken`, unless the operator creates it with `adopt` (offered in the
operator console after that refusal): the new tenant starts with none of
the old one's members or invites but keeps its review history, visible to
the new tenant's members. An installation name stays with the tenant that
first held it, even once that tenant is gone.

### Sealing key

A dashboard-managed tenant's secrets are sealed at rest with an instance
key, `KRITIK_DASHBOARD_KEY` / `dashboard.keySecret`: generate one with
`openssl rand -base64 32`. To rotate it, move the old value into
`KRITIK_DASHBOARD_OLD_KEYS` / `dashboard.oldKeysSecret` (comma-separated,
accepted only to open values already sealed under it), and set a freshly
generated value as `KRITIK_DASHBOARD_KEY`. A value sealed under an old key
is re-sealed under the current one the next time it is written, not
eagerly on rotation, so keep an old key listed until every value under it
has been touched at least once.

### `retention.transcripts`

A top-level `retention.transcripts` (default 30 days, minimum 24 hours)
controls how long an agentic review's full model transcript is kept; the
review itself, its findings and its comments outlive it. A transcript may
contain repository content the agent read while investigating, and it is
visible to every member of the tenant it belongs to, not only admins.

### Operational notes

- A dashboard tenant that fails to merge into the configuration at boot
  fails startup the same as a bad configuration file: fix the offending
  row or the file. A merge or apply failure after boot instead keeps the
  last good configuration running and raises the `kritik_config_error`
  gauge (labelled `merge` or `apply`) until a later attempt succeeds.
- A secret referenced by `file:` is only re-read when the configuration
  file itself changes, not on the referenced file's own schedule: rotate
  the file, then touch or reapply the configuration to pick it up.
- An `email:` operator, or an email invite, is only as trustworthy as the
  forge or IdP's own email verification — kritik does not verify addresses
  itself, it trusts what the sign-in reports.
- The web role only ever holds the application database DSN, never the
  owner DSN a migration or leader election needs, and refuses to start if
  it would.

## Evaluation

Review quality is measured offline: `mise run bench-mine` builds a corpus
of pull requests whose lines a later fix commit changed, and `mise run
bench` (with `OPENROUTER_API_KEY`) runs them through the service's own
fetch, context and prompt code in diff-only and full-context modes, reporting
recall on the expected findings, cost and latency per mode. See the ADR's
evaluation section.

## Development

Tool versions and tasks live in [`.mise/config.toml`](.mise/config.toml):

```sh
mise install
mise run build
mise run test               # unit tests
mise run test-integration   # store suite against a throwaway VectorChord container
mise run lint
```

### Metrics

The management port serves `/metrics` alongside `/healthz` and `/readyz`.
Beyond the Go runtime, every series is prefixed `kritik_` and labelled by
tenant, installation or model, never by pull request or commit:

| Series                                                                                 | Labels                | What it counts                                                                |
| -------------------------------------------------------------------------------------- | --------------------- | ----------------------------------------------------------------------------- |
| `kritik_webhooks_total`                                                                | installation, outcome | deliveries: enqueued, skipped, ignored, ping, unauthorized, unparsable, error |
| `kritik_polls_total`, `kritik_polled_pull_requests_total`                              | installation, outcome | backstop polls and the open pull requests they handed to ingest               |
| `kritik_reviews_total`, `kritik_review_duration_seconds`                               | tenant, status        | reviews by terminal status and wall time                                      |
| `kritik_findings_total`                                                                | tenant, severity      | findings posted                                                               |
| `kritik_followups_total`                                                               | tenant, outcome       | mentions handled: answered, limited, ignored, failed                          |
| `kritik_context_chunks_total`                                                          | tenant, stage         | context chunks put in front of the model                                      |
| `kritik_index_runs_total`, `kritik_index_chunks_total`                                 | tenant, mode, status  | index runs and chunks embedded                                                |
| `kritik_runner_runs_total`, `kritik_runner_duration_seconds`                           | tenant, kind, outcome | runner Jobs: success, failed, deadline                                        |
| `kritik_lease_wait_seconds`                                                            | tenant, model         | time waiting for a model concurrency slot                                     |
| `kritik_review_snoozes_total`                                                          | tenant, model         | reviews put back on the queue because every model slot was held               |
| `kritik_model_calls_total`, `kritik_model_tokens_total`, `kritik_model_cost_usd_total` | tenant, model, role   | calls; tokens by direction (input, cached, output); provider-reported cost    |
| `kritik_config_drift`                                                                  |                       | 1 while this replica's file differs from the applied one                      |

### Cluster development loop

Everything that touches a private cluster lives in the gitignored
`.private/` folder, so account names, secret references and cluster
pointers never enter the repository:

- `.private/deploy`: a kustomization for what the chart does not create
  (namespace, a CloudNativePG cluster with the `vector` extension via a
  CNPG `Database` resource and External Secrets `Password` generators for
  its roles, the bot and provider credentials as ExternalSecrets). It
  expects the CloudNativePG, External Secrets and Prometheus operators.
- `.private/values.yaml`: the chart values for that cluster; `mise run
deploy` installs `charts/kritik` with them and the freshly pushed image,
  so every dev loop exercises the chart.
- `.private/mise.local.toml`: `[env] KUBECONFIG = "..."`, symlinked from
  the repo root as `.mise.local.toml` so mise picks it up.
- `.private/image`: the last image reference pushed to `ttl.sh`.

```sh
mise run deploy     # build, push to ttl.sh (24h TTL), apply, wait for rollout
mise run logs       # follow the pod
mise run undeploy   # delete everything, database included
```

Images go to `ttl.sh` under a fresh random name on every deploy, so a
redeploy always pulls new code and nothing needs registry credentials. A
Helm chart will be the supported way to run kritik; `.private/deploy` is a
development harness, not an example to copy.

## License

[AGPL-3.0](LICENSE)
