# kritika

![Version](https://img.shields.io/static/v1?label=Version&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->
![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)
![AppVersion](https://img.shields.io/static/v1?label=AppVersion&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->

Self-hosted AI pull request reviewer for GitHub, backed by Postgres and per-review Kubernetes Jobs

**Homepage:** <https://github.com/home-operations/kritika>

## Usage

kritika ships as an OCI Helm chart. It needs a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) and pgvector, three
roles, its public URL, a way to sign in, and its configuration file:

```sh
helm install kritika oci://ghcr.io/home-operations/charts/kritika \
  --set database.host=kritika-postgres-rw \
  --set database.app.existingSecret=kritika-postgres-app \
  --set database.owner.existingSecret=kritika-postgres-credentials \
  --set database.runner.existingSecret=kritika-postgres-runner \
  --values my-values.yaml
```

where `my-values.yaml` sets:

```yaml
web:
  url: https://kritika.example.com
config:
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
env:
  KRITIKA_AUTH_ADMIN_PASSWORD:
    valueFrom:
      secretKeyRef: { name: kritika-admin, key: password }
  OPENROUTER_API_KEY:
    valueFrom:
      secretKeyRef: { name: kritika-openrouter, key: api-key }
# A Secret with the keys GITHUB_APP_PRIVATE_KEY and GITHUB_APP_WEBHOOK_SECRET.
envFrom:
  - secretRef:
      name: kritika-bot
ingress:
  enabled: true
  tls: [{ hosts: [kritika.example.com], secretName: kritika-tls }]
```

`web.url` is the one public URL: the dashboard at it, and the webhook
listener under it at `/hooks`, which the chart's Ingress or HTTPRoute
routes.

`config` is the whole configuration: sign-in (`auth`), the GitHub `apps`,
model `providers`, the `embedding`, the `defaults`, `repositories` entries
keyed `owner/*` or `owner/name`, and `accounts`. kritika reads it at
startup: the pods carry its checksum, so a change rolls them, and a pod
whose file doesn't load never becomes ready while the ones before it keep
serving. Secrets never go in it: it names environment variables, which
`env` and `envFrom` set from existing Secrets, the way any Kubernetes
container's environment is set. Every other `KRITIKA_*` variable goes in
`env` too, by name: here the local admin's password, which is the way into
a fresh instance; OIDC and GitHub sign-in with their role mappings, how
kritika runs (polling, retention, workers) and runner Jobs' deadline and
RuntimeClass are set the same way (see
[the configuration reference](https://github.com/home-operations/kritika/blob/main/docs/configuration.md)).
The chart refuses a key it derives from its other values, such as the
addresses or the database. The
[setup guide](https://github.com/home-operations/kritika/blob/main/docs/setup.md)
covers creating the GitHub App, and the dashboard's setup checklist shows
what a fresh instance still lacks.

### Database

kritika separates three Postgres roles and refuses to start otherwise: the
**owner** (runs migrations and applies configuration on the leader, must not
be a superuser), the **application** role (`database.app.role`, must not own
the tables so row-level security applies to it) and the **runner** role
(`database.runner.role`, handed to runner Jobs, can only write its own run).
The `vchord` (VectorChord) and `vector` (pgvector, whose types it builds on)
extensions must exist before the first start, and `vchord` must be in
`shared_preload_libraries`.

The owner and application connections must reach Postgres directly, or
through a pooler in session mode. The leader is whichever replica holds a
session advisory lock on its owner connection, and the dashboard's live
updates and the job queue's notifications `LISTEN` on the application
connection. A transaction-mode pooler, such as PgBouncer or a CloudNativePG
`Pooler` with `poolMode: transaction`, hands a session to other clients
between transactions, which breaks both. `kritika_leader` is 1 on the replica
that holds the lock.

[Postgres with CloudNativePG](https://kritika.home-operations.com/database/)
sets this up end to end: a two-instance cluster on TensorChord's VectorChord
image, the extensions through a `Database` resource, the application and
runner roles as `DatabaseRole` resources, whose username and password
Secrets are what `database.*.existingSecret` name, failover and backups.
kritika builds each connection from `database.host` and the role's Secret;
a Secret holding a connection URI instead names its key in `uriKey`.

### Egress gateway

The kritika serve pods serve a forward proxy on `service.gatewayPort`, and runner Jobs
are handed it as `HTTPS_PROXY` and `HTTP_PROXY`. With `networkPolicy.enabled`,
a runner pod can then reach nothing but DNS, Postgres and that port: its git
fetch and every command it runs go through the gateway, which allows a
destination by hostname only. github.com is always allowed once an app
is declared; `egress.allowHosts` in `config` adds the rest
(registries, release APIs), and `egress.credentials` names hosts the gateway
adds a bearer token to when a runner sends it a plain `http://` request, so
the runner never holds the token. The token is a secret reference like any
other in the file:

```yaml
config:
  egress:
    allowHosts: [api.github.com, "*.githubusercontent.com", ghcr.io]
    credentials:
      api.github.com: { env: GITHUB_TOKEN }
env:
  GITHUB_TOKEN:
    valueFrom:
      secretKeyRef: { name: kritika-github-token, key: token }
```

The same port is a runner's model endpoint. kritika serve mints a
token for each run, good for that run until its Job's deadline and revoked
when it ends, and hands it to the pod in place of a provider key; the
gateway answers each step through the account's provider with the key only
kritika serve holds, refuses a step once the run's token budget or the
account's `tokensPerMonth` is spent, and records the step's usage. Provider
endpoints are therefore not in a runner's allowlist. The runner also asks it
once for the review's similar code: the gateway embeds the diff with the
instance's embedder and answers from the repository's index, charging the
embedding to the run. Every review runs through the gateway, so it has no
switch.

### Runner tools

A repository's `agent.commands` lets the model run allowlisted
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
  image: ghcr.io/home-operations/kritika:<version>-tools
```

and, in `config`:

```yaml
config:
  egress:
    allowHosts: ["*.githubusercontent.com"]
  repositories:
    org-1/repo-1:
      agent: { commands: [gh, curl, fd, rg] }
```

A command runs without a shell, with an environment of `PATH`, its own
`HOME` and the gateway, `gh` also with the run's read-only token as
`GH_TOKEN`, and the runner makes itself unreadable to it first,
so a command cannot read the runner's credentials from `/proc`. The
`-tools` image does have one, though, and `fd -x` or `rg --pre` can start
it, and with it a script from the checkout: another reason to run runner
Jobs under a sandboxed RuntimeClass.

### Runner sandbox

Runner Jobs parse untrusted repository content and run what the model asks of
them. Set `KRITIKA_RUNNER_RUNTIME_CLASS` in `env` to a sandboxed runtime
the cluster offers (`gvisor` with runsc, or a Kata class) so a kernel
vulnerability reachable from the pod is contained by the sandbox rather than
the node. It is advised, not required: without it the pod's other bounds
still hold (no long-lived secret, egress by hostname through the gateway,
read-only root, no capabilities), but the container runtime alone separates
it from the node.

### Topology

The chart runs one Deployment of `kritika serve`, `replicas: 2`
by default with a PodDisruptionBudget. Every replica serves webhooks and the
dashboard and works jobs, and one holds the leader lock at a time; two keep
one serving while a rollout, such as the one a changed `config` starts,
replaces the other. Runner pods are the Jobs `kritika serve` creates, one per
review and index run, running `kritika run`. A replica that stops drains its
jobs first; one that dies outright leaves them to the leader, which hands
them back to the queue within a few minutes and deletes the runner Jobs they
left, and the remaining replica works them.

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| home-operations | <contact@home-operations.com> |  |

## Source Code

* <https://github.com/home-operations/kritika>

## Requirements

Kubernetes: `>=1.25.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for pod scheduling. |
| config | optional | `{}` | The configuration file, as YAML: the whole configuration, from `auth` and `apps` to `repositories` and `accounts`. Passed through verbatim, not tpl'd. See docs/configuration.md. |
| database.app.existingSecret | required | `""` | Secret holding the application role's credentials. |
| database.app.passwordKey | string | `"password"` | Key in that Secret holding the password. |
| database.app.role | string | `"kritika_app"` | Name of the application role, asserted at startup (not superuser, no BYPASSRLS, owns nothing). |
| database.app.uriKey | string | `""` | Key in that Secret holding a connection URI; set, it is used instead of the username, the password and `database.host`. |
| database.app.usernameKey | string | `"username"` | Key in that Secret holding the username. |
| database.connectTimeout | string | `"10s"` | Connection attempt timeout (KRITIKA_DATABASE_CONNECT_TIMEOUT), so a dial to a Service address DNS still caches after a redeploy fails fast and the retry looks the name up again. |
| database.host | string | `""` | Postgres host (KRITIKA_DATABASE_HOST), e.g. kritika-postgres-rw. Required unless every role's Secret holds a connection URI. |
| database.name | string | `"kritika"` | Database name (KRITIKA_DATABASE_NAME). |
| database.owner.existingSecret | required | `""` | Secret holding the owner role's credentials, used only by the leader for migrations and configuration sync. |
| database.owner.passwordKey | string | `"password"` | Key in that Secret holding the password. |
| database.owner.uriKey | string | `""` | Key in that Secret holding a connection URI; set, it is used instead of the username, the password and `database.host`. |
| database.owner.usernameKey | string | `"username"` | Key in that Secret holding the username. |
| database.port | int | `5432` | Postgres port (KRITIKA_DATABASE_PORT). |
| database.runner.existingSecret | required | `""` | Secret holding the runner role's credentials; referenced by runner Jobs, never read by kritika serve. |
| database.runner.passwordKey | string | `"password"` | Key in that Secret holding the password. |
| database.runner.role | string | `"kritika_runner"` | Name of the runner role, granted only what runner Jobs need. |
| database.runner.uriKey | string | `""` | Key in that Secret holding a connection URI; set, it is used instead of the username, the password and `database.host`. |
| database.runner.usernameKey | string | `"username"` | Key in that Secret holding the username. |
| database.sslmode | string | `"require"` | libpq sslmode (KRITIKA_DATABASE_SSLMODE). `require` encrypts without verifying the server, which an in-cluster Postgres with an operator-issued certificate offers without more setup. |
| deploymentAnnotations | object | `{}` | Annotations added to the Deployment (e.g. `reloader.stakater.com/auto: "true"`). Pod-level annotations go in `podAnnotations`. |
| env | object | `{}` | Environment variables of the kritika serve container, keyed by name: a plain value, or a map with the variable's `valueFrom` (a Secret, a ConfigMap or a field). Rendered through `tpl`. |
| envFrom | list | `[]` | Secrets and ConfigMaps loaded as environment variables in bulk, each key a variable: the Kubernetes `envFrom` list. A Secret whose keys are the variable names the configuration file references sets them all at once. |
| existingConfigMap | string | `""` | Existing ConfigMap holding the file under the `config.yaml` key; takes precedence over `config`. A change to it takes a restart. |
| fullnameOverride | string | `""` | Override the full release name. |
| httpRoute.annotations | object | `{}` | HTTPRoute annotations. |
| httpRoute.apiVersion | string | `""` | HTTPRoute apiVersion; empty defaults to gateway.networking.k8s.io/v1. |
| httpRoute.enabled | bool | `false` | Expose web.url through a Gateway API HTTPRoute. The host and path are web.url's and have no field of their own: GitHub delivers webhooks to that URL and sign-in redirects back to it, so a route for any other name would serve a dashboard that cannot sign in. |
| httpRoute.labels | object | `{}` | HTTPRoute labels. |
| httpRoute.parentRefs | list | `[]` | Gateways (and listeners) this route attaches to. |
| image.digest | string | `""` | Pin the image by digest (sha256:…); when set, overrides the tag. The release pipeline fills it with the published image's digest. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/home-operations/kritika"` | Image repository. |
| image.tag | string | `""` | Overrides the image tag; defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries, for the kritika serve pods and, through the runner ServiceAccount, runner Jobs and the tool images they mount. |
| ingress.annotations | object | `{}` | Ingress annotations. |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Expose web.url through an Ingress. The host and path are web.url's and have no field of their own: GitHub delivers webhooks to that URL and sign-in redirects back to it, so a route for any other name would serve a dashboard that cannot sign in. |
| ingress.tls | list | `[]` | Ingress TLS configuration, e.g. `[{hosts: [kritika.example.com], secretName: kritika-tls}]`. |
| livenessProbe | object | `{"httpGet":{"path":"/healthz","port":"metrics"},"periodSeconds":20}` | Liveness probe, on the metrics port. |
| logging.format | string | `"json"` | Log format: json or text. |
| logging.level | string | `"info"` | Log level: debug, info, warn or error. |
| monitoring.serviceMonitor.annotations | object | `{}` | ServiceMonitor annotations. |
| monitoring.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor for the metrics Service (requires its CRDs). |
| monitoring.serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| monitoring.serviceMonitor.labels | object | `{}` | ServiceMonitor labels. |
| monitoring.serviceMonitor.metricRelabelings | list | `[]` | Prometheus metric relabelings. |
| monitoring.serviceMonitor.relabelings | list | `[]` | Prometheus relabelings. |
| monitoring.serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| networkPolicy.allowDNS | bool | `true` | Allow DNS egress (UDP/TCP 53); the Cilium flavor allows it to kube-dns alone. |
| networkPolicy.egressPorts | list | `[443]` | TCP ports the service pods may egress to for forges and model endpoints. Runner pods reach the gateway alone. |
| networkPolicy.enabled | bool | `false` | Create the NetworkPolicies. |
| networkPolicy.type | string | `"default"` | Policy flavor for your CNI: "default" (networking.k8s.io/v1 NetworkPolicy), "cilium" (CiliumNetworkPolicy) or "calico" (projectcalico.org/v3 NetworkPolicy). |
| nodeSelector | object | `{}` | Node selector for pod scheduling. |
| podAnnotations | object | `{}` | Annotations added to the pods. |
| podDisruptionBudget.enabled | bool | `true` | Create a PodDisruptionBudget when there is more than one replica. |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum pods that may be unavailable, as a count or percentage; takes precedence over `minAvailable` when set. @schema type: [integer, string] @schema |
| podDisruptionBudget.minAvailable | string | `""` | Minimum pods that must stay available, as a count or percentage. Used unless `maxUnavailable` is set. @schema type: [integer, string] @schema |
| podLabels | object | `{}` | Labels added to the pods. |
| podSecurityContext | object | `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level securityContext (non-root uid/gid 65532, RuntimeDefault seccomp). |
| priorityClassName | string | `""` | PriorityClass for the pods. Empty uses the cluster default. |
| rbac.create | bool | `true` | Create the Role and RoleBinding kritika serve needs: Jobs in the release namespace, their pods and logs, and the Secrets it hands them. Nothing cluster-wide. |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"metrics"},"periodSeconds":10}` | Readiness probe, on the metrics port. A replica is ready once its configuration file has loaded and its listeners are up, before the database answers: until it does, the dashboard shows that kritika is starting and webhooks are refused with a reason, from kritika rather than the ingress. |
| replicas | int | `2` | Replicas of kritika serve. Every replica serves webhooks and the dashboard and works jobs; exactly one holds the leader lock at a time. Two keep one serving while a rollout replaces the other. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}` | Resource requests and limits of the kritika serve pods. |
| runner.image | string | `""` | Image for runner Jobs; empty uses the chart's image. The release's `-tools` tag (e.g. `ghcr.io/home-operations/kritika:1.2.3-tools`) adds curl, fd, gh and rg for an agentic review's `agent.commands`. |
| runner.resources | object | `{}` | Resources for runner pods (KRITIKA_RUNNER_RESOURCES), copied into the pod spec. |
| runner.serviceAccount.annotations | object | `{}` | Annotations for the runner ServiceAccount. |
| runner.serviceAccount.create | bool | `true` | Create the runner ServiceAccount: no permissions, no token mounted, and the chart's `imagePullSecrets` so runner Jobs can pull from a private registry. |
| runner.serviceAccount.name | string | `""` | Runner ServiceAccount name; generated from the release name if empty. |
| runner.tools | list | `[]` | Command-line tools a runner pod mounts from an image for the agent's run tool (KRITIKA_RUNNER_TOOLS), each a `name`, a digest-pinned `image`, the `path` of its binaries and the `commands` it provides. Needs Kubernetes 1.33 or newer, which mounts an image volume with a subPath; the chart refuses to render them on an older cluster. |
| runner.ttl | string | `"10m"` | How long a finished Job stays for kubectl before Kubernetes removes it (Go duration); the run row keeps everything the Job knew. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container securityContext (no privilege escalation, read-only root filesystem, drops ALL capabilities). |
| service.gatewayPort | int | `8082` | Port of the gateway the kritika serve pods run, on the pods and its Service: the forward proxy runner Jobs are handed as `HTTPS_PROXY`, allowing only the hosts the configuration names (github.com once an app is configured, `egress.allowHosts`), so runner pods need no direct internet egress, and the model and similar-code endpoints a runner calls with a per-run token, so no provider key enters a runner pod. |
| service.metricsPort | int | `8081` | Metrics and probe port. |
| service.port | int | `8080` | Public port: the webhooks (`POST /hooks/{app}`) and the dashboard. |
| service.type | string | `"ClusterIP"` | Service type of the public Service. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| serviceAccount.automount | bool | `true` | Automount the API token, which kritika serve needs to create runner Jobs. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount kritika serve runs as. |
| serviceAccount.name | string | `""` | ServiceAccount name; generated from the release name if empty. |
| startupProbe | object | `{"failureThreshold":30,"httpGet":{"path":"/healthz","port":"metrics"},"periodSeconds":2}` | Startup probe, on the metrics port. The liveness and readiness probes wait until it passes, so a pod still opening its listeners is not reported unready; it allows a minute. |
| strategy | object | `{"rollingUpdate":{"maxSurge":1,"maxUnavailable":0},"type":"RollingUpdate"}` | Deployment update strategy. A rolling update that surges one pod and takes none down keeps one replica serving while the other is replaced. Helm merges maps, so a switch to `Recreate` also sets `rollingUpdate: null`. |
| terminationGracePeriodSeconds | int | `150` | Grace period for a clean shutdown: kritika serve keeps accepting webhooks, dashboard requests and model steps for 5s while traffic moves off the pod, stops taking jobs and lets running ones finish for up to 100s, then retries the reviews it cut, and its gateway lets model steps in flight finish for up to 2m. |
| tests.image.pullPolicy | string | `"IfNotPresent"` | `helm test` image pull policy. |
| tests.image.repository | string | `"mirror.gcr.io/curlimages/curl"` | `helm test` connection-pod image; a gcr-mirrored curl, so the test never pulls from Docker Hub. |
| tests.image.tag | string | `"8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777"` | `helm test` image, pinned as `tag@sha256:digest` so Renovate bumps the tag and its digest together. |
| tolerations | list | `[]` | Tolerations for pod scheduling. |
| topologySpreadConstraints | list | `[{"labelSelector":{"matchLabels":{"app.kubernetes.io/instance":"{{ .Release.Name }}","app.kubernetes.io/name":"{{ include \"kritika.name\" . }}"}},"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway"}]` | Spread the kritika serve pods across nodes, so a node loss does not take both replicas: a soft constraint, so a one-node cluster still schedules them. Rendered through `tpl`; empty leaves scheduling to Kubernetes. |
| volumeMounts | list | `[]` | Additional volume mounts on the kritika serve container. |
| volumes | list | `[]` | Additional volumes on the Deployment. |
| web.url | required | `""` | Public URL the dashboard is reached at, e.g. https://kritika.example.com; the webhooks share it under `/hooks/<app name>`. Must be an absolute http(s) URL with no query or fragment. |

---

_This README is generated by [helm-docs](https://github.com/norwoodj/helm-docs) from `Chart.yaml` and `values.yaml`. Edit those (or `README.md.gotmpl`) and run `mise run generate`._
