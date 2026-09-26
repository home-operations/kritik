# Development

Tool versions and tasks live in [`.mise/config.toml`](../.mise/config.toml):

```sh
mise install
mise run build
mise run test               # unit tests
mise run test-integration   # store suite against a throwaway VectorChord container
mise run lint
```

## Evaluation

Review quality is measured offline: `mise run bench-mine` builds a corpus
of pull requests whose lines a later fix commit changed, and `mise run
bench` (with `OPENROUTER_API_KEY`) runs them through the service's own
fetch, context and prompt code in diff-only and full-context modes, reporting
recall on the expected findings, cost and latency per mode. See the ADR's
evaluation section.

## Cluster development loop

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
redeploy always pulls new code and nothing needs registry credentials.
The Helm chart is the supported way to run kritik; `.private/deploy` is a
development harness, not an example to copy.
