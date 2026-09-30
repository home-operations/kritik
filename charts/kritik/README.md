# kritik

![Version](https://img.shields.io/static/v1?label=Version&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->
![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)
![AppVersion](https://img.shields.io/static/v1?label=AppVersion&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->

Self-hosted AI pull request reviewer for GitHub, backed by Postgres and per-review Kubernetes Jobs

**Homepage:** <https://github.com/home-operations/kritik>

## Usage

kritik ships as an OCI Helm chart. It needs a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) and pgvector, three
roles, its public URL, a way to sign in, and its configuration file:

```sh
helm install kritik oci://ghcr.io/home-operations/charts/kritik \
  --set database.app.existingSecret=kritik-postgres-app \
  --set database.owner.existingSecret=kritik-postgres-credentials \
  --set database.runner.existingSecret=kritik-postgres-runner \
  --values my-values.yaml
```

where `my-values.yaml` sets:

```yaml
web:
  url: https://kritik.example.com
auth:
  admin:
    passwordSecret: { name: kritik-admin }
config:
  file:
    apps:
      - name: github
        accounts: [org-1]
        clientId: Iv1.example
        privateKey: { env: GITHUB_APP_PRIVATE_KEY }
        webhookSecret: { env: GITHUB_APP_WEBHOOK_SECRET }
    providers:
      openrouter:
        type: openrouter
        apiKey: { env: OPENROUTER_API_KEY }
    defaults:
      models: { review: openrouter/vendor/large-model }
secretEnv:
  - { name: GITHUB_APP_PRIVATE_KEY, secretName: kritik-bot, key: private-key.pem }
  - { name: GITHUB_APP_WEBHOOK_SECRET, secretName: kritik-bot, key: webhook-secret }
  - { name: OPENROUTER_API_KEY, secretName: kritik-openrouter, key: api-key }
ingress:
  enabled: true
  tls: [{ hosts: [kritik.example.com], secretName: kritik-tls }]
```

`web.url` is the one public URL: the dashboard at it, and the webhook
listener under it at `/hooks`, which the chart's Ingress or HTTPRoute
routes. `auth` renders the `KRITIK_AUTH_*` variables: here the local admin,
with its password from an existing Secret, which is the way into a fresh
instance. OIDC and GitHub sign-in, with role mappings, set the same way
(see [the configuration reference](https://github.com/home-operations/kritik/blob/main/docs/configuration.md)).

`config.file` is the whole configuration: the GitHub `apps`, model
`providers`, the `embedding`, the `defaults`, `repositories` entries keyed
`owner/*` or `owner/name`, and `accounts`. How kritik runs, polling,
retention and runner Jobs, is set by values instead (`config.pollInterval`,
`runner.deadline`, `runner.tools` and the rest below). kritik reads it
at startup: the pods carry its checksum, so a change rolls them, and a
pod whose file doesn't load never becomes ready while the ones before it
keep serving. Secrets never go in it: it names
environment variables, which `secretEnv` sets from existing Secrets. The
[setup guide](https://github.com/home-operations/kritik/blob/main/docs/setup.md)
covers creating the GitHub App, and the dashboard's setup checklist shows
what a fresh instance still lacks.

### Database

kritik separates three Postgres roles and refuses to start otherwise: the
**owner** (runs migrations and applies configuration on the leader, must not
be a superuser), the **application** role (`database.app.role`, must not own
the tables so row-level security applies to it) and the **runner** role
(`database.runner.role`, handed to runner Jobs, can only write its own run).
The `vchord` (VectorChord) and `vector` (pgvector, whose types it builds on)
extensions must exist before the first start, and `vchord` must be in
`shared_preload_libraries`.

On CloudNativePG, use TensorChord's image (`ghcr.io/tensorchord/cloudnative-vectorchord`,
tagged `<postgres>-<vchord>`) or mount `ghcr.io/tensorchord/vchord-scratch` as an
image-volume extension, load the library, make the bootstrap owner `owner`,
declare the other two roles under `spec.managed.roles`, and let a `Database`
resource create the extensions:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: kritik-postgres
spec:
  instances: 1
  imageName: ghcr.io/cloudnative-pg/postgresql:18-standard-trixie
  enableSuperuserAccess: false
  bootstrap:
    initdb:
      database: kritik
      owner: kritik
      secret:
        name: kritik-postgres-credentials
  managed:
    roles:
      - name: kritik_app
        login: true
        passwordSecret:
          name: kritik-postgres-app
      - name: kritik_runner
        login: true
        passwordSecret:
          name: kritik-postgres-runner
---
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: kritik
spec:
  name: kritik
  owner: kritik
  cluster:
    name: kritik-postgres
  extensions:
    - name: vector
      ensure: present
    - name: vchord
      ensure: present
```

with, on the `Cluster`:

```yaml
spec:
  imageName: ghcr.io/tensorchord/cloudnative-vectorchord:18.6-1.1.1
  postgresql:
    shared_preload_libraries:
      - vchord
```

Each `passwordSecret` is a basic-auth Secret; kritik reads a `uri` key from
the Secrets named in `database.*.existingSecret`, so either use CNPG's
generated `uri` for the owner or add one for the managed roles (an External
Secrets `Password` generator plus a templated `uri` works).

### Egress gateway

The kritik serve pods serve a forward proxy on `gateway.port`, and runner Jobs
are handed it as `HTTPS_PROXY` and `HTTP_PROXY`. With `networkPolicy.enabled`,
a runner pod can then reach nothing but DNS, Postgres and that port: its git
fetch and every command it runs go through the gateway, which allows a
destination by hostname only. github.com is always allowed once an app
is declared; `egress.allowHosts` in `config.file` adds the rest
(registries, release APIs), and `egress.credentials` names hosts the gateway
adds a bearer token to when a runner sends it a plain `http://` request, so
the runner never holds the token. The token is a secret reference like any
other in the file:

```yaml
config:
  file:
    egress:
      allowHosts: [api.github.com, "*.githubusercontent.com", ghcr.io]
      credentials:
        api.github.com: { env: GITHUB_TOKEN }
secretEnv:
  - { name: GITHUB_TOKEN, secretName: kritik-github-token, key: token }
```

The same port is an agentic runner's model endpoint. kritik serve mints a
token for each run, good for that run until its Job's deadline and revoked
when it ends, and hands it to the pod in place of a provider key; the
gateway answers each step through the account's provider with the key only
kritik serve holds, refuses a step once the run's token budget or the
account's `tokensPerMonth` is spent, and records the step's usage. Provider
endpoints are therefore not in a runner's allowlist.

`gateway.enabled: false` removes the listener and the Service and gives runner
pods the `networkPolicy.egressPorts` to anywhere instead. Agentic reviews are
refused without the gateway.

### Runner tools

An agentic repository's `agent.commands` lets the model run allowlisted
binaries over a checkout of the head commit: to read a dependency bump's
release notes and compare view with `gh`, fetch anything else with
`curl`, or search with `rg` and `fd`. The chart's image has none of them,
so no command is offered; point `runner.image` at the release's `-tools`
tag, an Alpine image with all four. `gh` signs in with the run's own
token, which can read only the repository under review and public
repositories, and an app already allows `github.com` and `api.github.com`
through the gateway. Every other host `curl` reaches must pass the
gateway too, so add the release hosts and registries to
`egress.allowHosts`:

```yaml
runner:
  image: ghcr.io/home-operations/kritik:<version>-tools
```

and, in `config.file`:

```yaml
config:
  file:
    egress:
      allowHosts: ["*.githubusercontent.com"]
    repositories:
      org-1/repo-1:
        mode: agentic
        agent: { commands: [gh, curl, fd, rg] }
```

A command runs without a shell, with an environment of `PATH`, its own
`HOME` and the gateway, `gh` also with the run's read-only token as
`GH_TOKEN`, and the runner makes itself unreadable to it first,
so a command cannot read the runner's credentials from `/proc`. The
`-tools` image does have one, though, and `fd -x` or `rg --pre` can start
it, and with it a script from the checkout: another reason to run runner
Jobs under a sandboxed `runner.runtimeClassName`.

### Runner sandbox

Runner Jobs parse untrusted repository content and, in agentic mode, run what
the model asks of them. Set `runner.runtimeClassName` to a sandboxed runtime
the cluster offers (`gvisor` with runsc, or a Kata class) so a kernel
vulnerability reachable from the pod is contained by the sandbox rather than
the node. It is advised, not required: without it the pod's other bounds
still hold (no long-lived secret, egress by hostname through the gateway,
read-only root, no capabilities), but the container runtime alone separates
it from the node.

### Topology

The chart runs one Deployment of `kritik serve` (ADR-0024), `replicas: 2`
by default with a PodDisruptionBudget. Every replica serves webhooks and the
dashboard and works jobs, and one holds the leader lock at a time; two keep
one serving while a rollout, such as the one a changed `config.file` starts,
replaces the other. Runner pods are the Jobs `kritik serve` creates, one per
review and index run, running `kritik run`.

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| home-operations | <contact@home-operations.com> |  |

## Source Code

* <https://github.com/home-operations/kritik>

## Requirements

Kubernetes: `>=1.25.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for pod scheduling. |
| auth.admin.passwordSecret.key | string | `"password"` | Key in that Secret. |
| auth.admin.passwordSecret.name | string | `""` | Existing Secret holding the local admin's password; the local admin exists only while one is set. |
| auth.admin.user | string | `""` | The local admin's username; empty is `admin`. |
| auth.github.clientId | string | `""` | Client ID of an OAuth App, or of a GitHub App, to sign in with GitHub. |
| auth.github.clientSecretSecret.key | string | `"client-secret"` | Key in that Secret. |
| auth.github.clientSecretSecret.name | string | `""` | Existing Secret holding the client secret. |
| auth.github.roleMappingExpr | string | `""` | CEL expression giving a role, or a map of accounts to roles (KRITIK_AUTH_GITHUB_ROLE_MAPPING_EXPR). |
| auth.oidc.clientId | string | `""` | OAuth client ID at the issuer. |
| auth.oidc.clientSecretSecret.key | string | `"client-secret"` | Key in that Secret. |
| auth.oidc.clientSecretSecret.name | string | `""` | Existing Secret holding the client secret. |
| auth.oidc.defaultRole | string | `""` | Role when the mapping places nobody: none (the default) or member. |
| auth.oidc.issuer | string | `""` | OIDC issuer (https); set, with a client, to sign in through it. |
| auth.oidc.name | string | `""` | The sign-in button's label; empty is "SSO". |
| auth.oidc.roleMappingExpr | string | `""` | CEL expression giving a role, or a map of accounts to roles (KRITIK_AUTH_OIDC_ROLE_MAPPING_EXPR, docs/configuration.md). |
| auth.oidc.rolesClaim | string | `""` | ID token or UserInfo claim a role mapping reads as `roles`. |
| auth.oidc.scopes | list | `[]` | Scopes to request; empty is openid, email and profile. |
| auth.sessionTTL | string | `""` | How long a dashboard session lasts (Go duration, 5m to 720h); empty is 12h. |
| config.existingConfigMap | string | `""` | Existing ConfigMap holding the file under the `config.yaml` key; takes precedence over `file`. A change to it takes a restart. |
| config.extraEnv | list | `[]` | Extra raw env vars merged into every role's container (advanced). |
| config.file | optional | `{}` | The configuration file, as YAML: the whole configuration, from `auth` and `apps` to `repositories` and `accounts`. Passed through verbatim, not tpl'd. See docs/configuration.md. |
| config.indexGrace | string | `""` | How long the index of a repository that stopped running is kept (KRITIK_INDEX_GRACE, Go duration). Empty is kritik's default, 720h. |
| config.indexWorkers | int | `1` | Index jobs one replica runs at once (KRITIK_INDEX_WORKERS), rate-limited apart from reviews. |
| config.logFormat | string | `"json"` | Log format: json or text. |
| config.logLevel | string | `"info"` | Log level: debug, info, warn or error. |
| config.onboardWindow | int | `0` | How many onboarding index jobs the leader keeps queued or running at once (KRITIK_ONBOARD_WINDOW). 0 is kritik's default, 4. |
| config.pollInterval | string | `""` | How often the leader lists each app's open pull requests, its backstop for missed webhooks (KRITIK_POLL_INTERVAL, Go duration); `0s` turns polling off. Empty is kritik's default, 10m. |
| config.pollLookback | string | `""` | How far back a first or long-idle poll looks (KRITIK_POLL_LOOKBACK, Go duration). Empty is kritik's default, 24h. |
| config.reviewWorkers | int | `2` | Review jobs one replica runs at once (KRITIK_REVIEW_WORKERS); follow-ups share the count. A review or index job holds at most one runner pod, so runner pods never exceed `replicas` × (reviewWorkers + indexWorkers). |
| config.transcriptRetention | string | `""` | How long an agentic review's transcript is kept, at least 24h (KRITIK_TRANSCRIPT_RETENTION, Go duration). Empty is kritik's default, 720h. |
| database.app.existingSecret | required | `""` | Secret holding the application role's connection URI. |
| database.app.key | string | `"uri"` | Key in that Secret. |
| database.app.role | string | `"kritik_app"` | Name of the application role, asserted at startup (not superuser, no BYPASSRLS, owns nothing). |
| database.owner.existingSecret | required | `""` | Secret holding the owner role's connection URI, used only by the leader for migrations and configuration sync. |
| database.owner.key | string | `"uri"` | Key in that Secret. |
| database.runner.existingSecret | required | `""` | Secret holding the runner role's connection URI; referenced by runner Jobs, never read by kritik serve. |
| database.runner.key | string | `"uri"` | Key in that Secret. |
| database.runner.role | string | `"kritik_runner"` | Name of the runner role, granted only what runner Jobs need. |
| deploymentAnnotations | object | `{}` | Annotations added to every Deployment (e.g. `reloader.stakater.com/auto: "true"`). Pod-level annotations go in `podAnnotations`. |
| fullnameOverride | string | `""` | Override the full release name. |
| gateway.enabled | bool | `true` | Serve the gateway on the kritik serve pods: the forward proxy runner Jobs are handed as `HTTPS_PROXY`, allowing only the hosts the configuration names (github.com once an app is configured, `egress.allowHosts`), so runner pods need no direct internet egress (ADR-0008), and the model endpoint an agentic runner calls with a per-run token, so no provider key enters a runner pod (ADR-0004). Agentic reviews, the default mode, are refused without it: set `KRITIK_DEFAULTS_MODE=single` (or `defaults.mode: single`) before turning it off. |
| gateway.port | int | `8082` | Gateway port on the pods and its Service. |
| httpRoute.annotations | object | `{}` | HTTPRoute annotations. |
| httpRoute.apiVersion | string | `""` | HTTPRoute apiVersion; empty defaults to gateway.networking.k8s.io/v1. |
| httpRoute.enabled | bool | `false` | Expose web.url through a Gateway API HTTPRoute. |
| httpRoute.labels | object | `{}` | HTTPRoute labels. |
| httpRoute.parentRefs | list | `[]` | Gateways (and listeners) this route attaches to. |
| image.digest | string | `""` | Pin the image by digest (sha256:…); when set, overrides the tag. The release pipeline fills it with the published image's digest. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/home-operations/kritik"` | Image repository. |
| image.tag | string | `""` | Overrides the image tag; defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries. |
| ingress.annotations | object | `{}` | Ingress annotations. |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Expose web.url through an Ingress. |
| ingress.tls | list | `[]` | Ingress TLS configuration, e.g. `[{hosts: [kritik.example.com], secretName: kritik-tls}]`. |
| livenessProbe | object | `{"httpGet":{"path":"/healthz","port":"metrics"},"periodSeconds":20}` | Liveness probe, on the metrics port. |
| monitoring.serviceMonitor.annotations | object | `{}` | ServiceMonitor annotations. |
| monitoring.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor for every role's metrics (requires its CRDs). |
| monitoring.serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| monitoring.serviceMonitor.labels | object | `{}` | ServiceMonitor labels. |
| monitoring.serviceMonitor.metricRelabelings | list | `[]` | Prometheus metric relabelings. |
| monitoring.serviceMonitor.relabelings | list | `[]` | Prometheus relabelings. |
| monitoring.serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| networkPolicy.allowDNS | bool | `true` | Allow DNS egress (UDP/TCP 53). |
| networkPolicy.egressPorts | list | `[443]` | TCP ports the service pods may egress to for forges and model endpoints. Runner pods get these only when the gateway is disabled; with it, they reach the gateway alone. |
| networkPolicy.enabled | bool | `false` | Create the NetworkPolicies. |
| networkPolicy.postgresPort | int | `5432` | Postgres port allowed for egress. |
| nodeSelector | object | `{}` | Node selector for pod scheduling. |
| podAnnotations | object | `{}` | Annotations added to the pods. |
| podDisruptionBudget.enabled | bool | `true` | Create a PodDisruptionBudget when there is more than one replica. |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum pods that may be unavailable, as a count or percentage. @schema type: [integer, string] @schema |
| podLabels | object | `{}` | Labels added to the pods. |
| podSecurityContext | object | `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level securityContext (non-root uid/gid 65532, RuntimeDefault seccomp). |
| priorityClassName | string | `""` | PriorityClass for the pods. Empty uses the cluster default. |
| rbac.create | bool | `true` | Create the Role and RoleBinding kritik serve needs: Jobs in the release namespace, their pods and logs, and the Secrets it hands them. Nothing cluster-wide. |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"metrics"},"periodSeconds":10}` | Readiness probe, on the metrics port. A replica is ready once it has a database connection and its listeners are up. |
| replicas | int | `2` | Replicas of kritik serve. Every replica serves webhooks and the dashboard and works jobs; exactly one holds the leader lock at a time. Two keep one serving while a rollout replaces the other (ADR-0022 §2.3). |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}` | Resource requests and limits of the kritik serve pods. |
| runner.deadline | string | `""` | Deadline of a runner Job (KRITIK_RUNNER_DEADLINE, Go duration). Empty is kritik's default, 15m. |
| runner.image | string | `""` | Image for runner Jobs; empty uses the chart's image. The release's `-tools` tag (e.g. `ghcr.io/home-operations/kritik:1.2.3-tools`) adds curl, fd, gh and rg for an agentic review's `agent.commands`. |
| runner.resources | object | `{}` | Resources for runner pods (KRITIK_RUNNER_RESOURCES), copied into the pod spec. |
| runner.runtimeClassName | string | `""` | RuntimeClass for runner Jobs (e.g. `gvisor`, `kata`). Advised: a runner parses untrusted repository content and, in agentic mode, runs what the model asks; a sandboxed runtime keeps it from the node's kernel. Empty uses the cluster default. |
| runner.serviceAccount.annotations | object | `{}` | Annotations for the runner ServiceAccount. |
| runner.serviceAccount.create | bool | `true` | Create the runner ServiceAccount (no permissions, no token mounted). |
| runner.serviceAccount.name | string | `""` | Runner ServiceAccount name; generated from the release name if empty. |
| runner.tools | list | `[]` | Command-line tools a runner pod mounts from an image for the agent's run tool (KRITIK_RUNNER_TOOLS, ADR-0011), each a `name`, a digest-pinned `image`, the `path` of its binaries and the `commands` it provides. |
| runner.ttl | string | `"1h"` | How long a finished Job stays for kubectl before Kubernetes removes it (Go duration); the run row keeps everything the Job knew. |
| secretEnv | list | `[]` | Environment variables set from existing Secrets, for the configuration file's `{ env: NAME }` references. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container securityContext (no privilege escalation, read-only root filesystem, drops ALL capabilities). |
| service.metricsPort | int | `8081` | Metrics and probe port. |
| service.port | int | `8080` | Public port: the webhooks (`POST /hooks/{app}`) and the dashboard (ADR-0024 §2.2). |
| service.type | string | `"ClusterIP"` | Service type of the public Service. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| serviceAccount.automount | bool | `true` | Automount the API token, which kritik serve needs to create runner Jobs. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount kritik serve runs as. |
| serviceAccount.name | string | `""` | ServiceAccount name; generated from the release name if empty. |
| startupProbe | object | `{"failureThreshold":30,"httpGet":{"path":"/healthz","port":"metrics"},"periodSeconds":2}` | Startup probe, on the metrics port. The liveness and readiness probes wait until it passes, so a pod still opening its listeners is not reported unready; it allows a minute. |
| terminationGracePeriodSeconds | int | `150` | Grace period for a clean shutdown: kritik serve stops taking jobs and lets running ones finish for up to 100s, then retries the reviews it cut, and its gateway lets model steps in flight finish for up to 2m. |
| tolerations | list | `[]` | Tolerations for pod scheduling. |
| volumeMounts | list | `[]` | Additional volume mounts on every container. |
| volumes | list | `[]` | Additional volumes on every Deployment. |
| web.url | required | `""` | Public URL the dashboard is reached at, e.g. https://kritik.example.com; the webhooks share it under `/hooks/<app name>`. Must be an absolute http(s) URL with no query or fragment. |

---

_This README is generated by [helm-docs](https://github.com/norwoodj/helm-docs) from `Chart.yaml` and `values.yaml`. Edit those (or `README.md.gotmpl`) and run `mise run generate`._
