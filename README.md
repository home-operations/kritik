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
amended by [`docs/adr/0003-agentic-review-loop.md`](docs/adr/0003-agentic-review-loop.md),
which turns the single-shot review into an agent loop in the runner pod
with the worker as its model gateway; ADR-0001 is kept as historical input.
What works today: the configuration file loader with live reload, the
Postgres store with row-level security and River, the ingest role (a signed
forge webhook becomes rows and a review job), and the worker role, which
takes the merge-base from the forge, runs a Kubernetes Job per review
that fetches the two commits, diffs them and writes a context pack, then
holds a per-tenant model lease, asks the configured OpenRouter model for
structured findings, and writes back a sticky summary comment, inline
review comments and a commit status. The runner also builds the context
the prompt carries beyond the diff: whole declarations the diff touches,
definitions of identifiers on changed lines, and callers of changed
declarations, all cut by tree-sitter (pure Go, every grammar embedded;
the stripped static binary is about 122 MiB). With a deployment embedder
configured (`KRITIK_EMBED_*`), the leader onboards every declared
repository into a pgvector index of the default branch, default-branch
pushes advance it incrementally, and reviews add the most similar indexed
chunks as a fourth context stage. An @-mention of the bot by someone with
write access gets an answer in the thread, with the diff, the review's
context and findings, and the thread in the prompt, five per pull request
per hour. The leader also polls each installation's open pull requests on
an interval as a backstop for missed webhooks; a head the webhook already
enqueued is deduplicated by the job queue. The other forges, the Helm
chart and the evaluation harness come next; see the ADR for the rest.

## Installing

kritik ships as an OCI Helm chart, `oci://ghcr.io/home-operations/charts/kritik`.
The chart's [README](charts/kritik/README.md) lists every value and shows the
CloudNativePG setup for the three database roles. In short: a pgvector
Postgres with an owner, an application and a runner role, the configuration
file under `config.file`, the secrets it references under `secretMounts`, and
optionally an embedder under `embedding` for the index. `roles.all` runs the
single-process topology; `roles.ingest` and `roles.worker` split it.

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
mise run test-integration   # store suite against a throwaway pgvector container
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
