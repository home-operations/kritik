# kritika

![Version](https://img.shields.io/static/v1?label=Version&message=0.0.3&color=informational&style=flat-square) <!-- x-release-please-version -->
![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)
![AppVersion](https://img.shields.io/static/v1?label=AppVersion&message=0.0.3&color=informational&style=flat-square) <!-- x-release-please-version -->

Self-hosted AI pull request reviewer for GitHub, backed by Postgres and per-review Kubernetes Jobs

**Homepage:** <https://github.com/home-operations/kritika>

## Usage

kritika ships as an OCI Helm chart. It needs a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) and pgvector, three
roles, its public URL, a way to sign in, and its configuration file:

```sh
helm install kritika oci://ghcr.io/home-operations/charts/kritika \
  --set database.app.existingSecret=kritika-postgres-app \
  --set database.owner.existingSecret=kritika-postgres-credentials \
  --set database.runner.existingSecret=kritika-postgres-runner \
  --values my-values.yaml
```

where `my-values.yaml` sets:

```yaml
config:
  webUrl: https://kritika.example.com
  authAdminPassword:
    valueFrom:
      secretKeyRef: { name: kritika-admin, key: password }
  providersApiKey:
    valueFrom:
      secretKeyRef: { name: kritika-openrouter, key: api-key }
  defaultsModelsReview: openrouter/vendor/large-model
configFile:
  apps:
    github:
      accounts: [org-1]
      clientId: Iv1.example
      privateKey: { env: GITHUB_APP_PRIVATE_KEY }
      webhookSecret: { env: GITHUB_APP_WEBHOOK_SECRET }
# A Secret with the keys GITHUB_APP_PRIVATE_KEY and GITHUB_APP_WEBHOOK_SECRET.
envFrom:
  - secretRef:
      name: kritika-bot
ingress:
  enabled: true
  hosts: [{ host: kritika.example.com, paths: [{ path: / }] }]
  tls: [{ hosts: [kritika.example.com], secretName: kritika-tls }]
```

`config` is every `KRITIKA_*` variable, keyed by its name without the
prefix in camelCase (`pollInterval` is `KRITIKA_POLL_INTERVAL`): `webUrl`,
the one public URL, with the dashboard at it and the webhook listener
under it at `/hooks`; logging, the workers, polling, retention and runner
Jobs' deadline and RuntimeClass; and the variables that stand in for the
configuration file's sections, sign-in, one app, one provider, the default
models and the embedder (see
[the configuration reference](https://github.com/home-operations/kritika/blob/main/docs/configuration.md)).
A key left empty sets nothing and kritika's default applies; a secret
takes a `valueFrom` map, like the admin password above, which is the way
into a fresh instance. The chart's Ingress or HTTPRoute must route
`webUrl`'s host.

`configFile` is what is reviewed and how: sign-in (`auth`), the GitHub
`apps`, model `providers`, the `embedding`, the `defaults`, `repositories`
entries keyed `owner/*` or `owner/name`, and `accounts`, with the
`config` variables winning over it. kritika reads it at startup: the pods
carry its checksum, so a change rolls them, and a pod whose file doesn't
load never becomes ready while the ones before it keep serving. Secrets
never go in it: it names environment variables, which `env` and `envFrom`
set from existing Secrets, the way any Kubernetes container's environment
is set. The chart refuses a `KRITIKA_*` variable under `env`, naming its
`config` key, and a `config` key for a variable it derives from its other
values, such as the addresses or the database. The
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
runner roles as `DatabaseRole` resources, the Secrets with their `uri` key
for `database.*.existingSecret`, failover and backups.

### Egress gateway

The kritika serve pods serve a forward proxy on `service.gatewayPort`, and runner Jobs
are handed it as `HTTPS_PROXY` and `HTTP_PROXY`. With `networkPolicy.enabled`,
a runner pod can then reach nothing but DNS, Postgres and that port: its git
fetch and every command it runs go through the gateway, which allows a
destination by hostname only. github.com is always allowed once an app
is declared; `egress.allowHosts` in `configFile` adds the rest
(registries, release APIs), and `egress.credentials` names hosts the gateway
adds a bearer token to when a runner sends it a plain `http://` request, so
the runner never holds the token. The token is a secret reference like any
other in the file:

```yaml
configFile:
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
so no command is offered; enable `runner.toolsImage` to run runner Jobs
on the release's `-tools` image, an Alpine image with all four, pinned by
digest like the chart's image. `gh` signs in with the run's own
token, which can read only the repository under review and public
repositories, and an app already allows `github.com` and `api.github.com`
through the gateway. Every other host `curl` reaches must pass the
gateway too, so add the release hosts and registries to
`egress.allowHosts`:

```yaml
runner:
  toolsImage:
    enabled: true
```

and, in `configFile`:

```yaml
configFile:
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
them. Set `config.runnerRuntimeClass` to a sandboxed runtime
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
one serving while a rollout, such as the one a changed `configFile` starts,
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
| config.appsAccounts | string | `""` | The users and organizations the app serves, comma-separated; set it to declare the app here. |
| config.appsClientId | string | `""` | The GitHub App's client id, inline or from a Secret. @schema type: [string, object] @schema |
| config.appsName | string | `""` | The app's name, which is its webhook path, `/hooks/<name>`; `github` unless set. |
| config.appsPrivateKey | string | `""` | The GitHub App's private key (PEM), from a Secret. @schema type: [string, object] @schema |
| config.appsWebhookSecret | string | `""` | The GitHub App's webhook secret, from a Secret. @schema type: [string, object] @schema |
| config.authAdminPassword | string | `""` | The local admin's password, from a Secret. The local admin exists only while one is set: the way into a fresh instance, and a way in when every provider is down. @schema type: [string, object] @schema |
| config.authAdminUser | string | `""` | The local admin's username; `admin` unless set. |
| config.authGithubClientId | string | `""` | Client id for signing in with GitHub: an OAuth App's, or the GitHub App's own; set it to sign in through GitHub. |
| config.authGithubClientSecret | string | `""` | Client secret for signing in with GitHub, from a Secret. @schema type: [string, object] @schema |
| config.authGithubRoleMappingExpr | string | `""` | CEL expression mapping a GitHub sign-in to a kritika role, e.g. `login == "user-1" ? "admin" : ""`. |
| config.authOidcClientId | string | `""` | OIDC client id. |
| config.authOidcClientSecret | string | `""` | OIDC client secret, from a Secret. @schema type: [string, object] @schema |
| config.authOidcDefaultRole | string | `""` | Role of an OIDC sign-in the mapping gives none; none unless set. |
| config.authOidcIssuer | string | `""` | OpenID Connect issuer, an https URL; set it to sign in through OIDC. |
| config.authOidcName | string | `""` | Label of the OIDC provider on the sign-in page; "SSO" unless set. |
| config.authOidcRoleMappingExpr | string | `""` | CEL expression mapping an OIDC sign-in to a kritika role, `admin`, `member` or "" for none, e.g. `"kritika-admins" in roles ? "admin" : "member"`. |
| config.authOidcRolesClaim | string | `""` | The ID token claim holding the person's roles or groups, read by `authOidcRoleMappingExpr`. |
| config.authOidcScopes | string | `""` | OIDC scopes, comma-separated; `openid,email,profile` unless set. |
| config.authSessionTtl | string | `""` | How long a dashboard session lasts, between 5m and 720h; 12h unless set. |
| config.defaultsFeedback | string | `""` | How much a review says: `detailed` (nits, missing tests and questions inline), `standard` (nits in the summary only) or `minimal` (bugs, risks and breaking changes only); `standard` unless set. |
| config.defaultsForks | string | `""` | Review pull requests from forks without being asked; `false` unless set, when one is reviewed only when a maintainer comments `@<app slug> review`. @schema type: [boolean, string] @schema |
| config.defaultsModelsFallback | string | `""` | The model a review falls back to when the review model fails. |
| config.defaultsModelsReview | string | `""` | The model every review runs on unless a repository names another, `<provider>/<model>`. |
| config.defaultsSettle | string | `""` | How long a review waits after a push, so a burst of pushes collapses onto the last one before anything is spent, e.g. `30s`; immediate unless set. |
| config.diffRetention | string | `""` | How long a review keeps the diff it was made from, the context it read and the repository files it named, at least 24h; 720h unless set. |
| config.embeddingDims | string | `""` | The embedding's dimensions, which the model must produce. @schema type: [integer, string] @schema |
| config.embeddingModel | string | `""` | The embedding model, `<provider>/<model>`; set it to index each repository for similar code. |
| config.gatewayTokenTtl | string | `""` | How long a run's gateway token outlives its Job's deadline, in case the replica that minted it dies before revoking it; 1h unless set. |
| config.indexGrace | string | `""` | How long the index of a repository that stopped running is kept; 720h unless set. |
| config.indexWorkers | string | `""` | Index jobs one replica runs at once, rate-limited apart from reviews so onboarding a large account cannot starve them; 1 unless set. @schema type: [integer, string] @schema |
| config.leaderRetryInterval | string | `""` | How often a replica retries the leader lock, and the holder checks it still has it; 15s unless set. |
| config.logFormat | string | `"json"` | Log format: json or text. |
| config.logLevel | string | `"info"` | Log level: debug, info, warn or error. |
| config.onboardWindow | string | `""` | Onboarding index jobs the leader keeps queued or running at once; 4 unless set. @schema type: [integer, string] @schema |
| config.pollInterval | string | `""` | How often the leader lists each app's open pull requests, its backstop for missed webhooks; `0s` turns it off; 10m unless set. |
| config.pollLookback | string | `""` | How far back a first or long-idle poll looks; 24h unless set. |
| config.providersApiKey | string | `""` | The provider's API key, from a Secret; set it to declare the provider here. @schema type: [string, object] @schema |
| config.providersBaseUrl | string | `""` | The provider's base URL, for an OpenAI-compatible endpoint; the type's own unless set. |
| config.providersName | string | `""` | The provider's name, which models are addressed through as `<name>/<model>`; `openrouter` unless set. |
| config.providersRetries | string | `""` | How many more times a review's model step is tried when the provider fails it in a way another attempt may not (a 5xx, a 429, a timeout), with backoff; 0 unless set, at most 5. @schema type: [integer, string] @schema |
| config.providersType | string | `""` | The provider's type, `openrouter`, `openai` or `anthropic`; the name unless set, when the name is one of those. |
| config.reviewWorkers | string | `""` | Review jobs one replica runs at once, each holding a runner pod open; 2 unless set. @schema type: [integer, string] @schema |
| config.runnerDeadline | string | `""` | A runner Job's deadline; 15m unless set. |
| config.runnerRuntimeClass | string | `""` | RuntimeClass runner Jobs run under, e.g. `gvisor` or a Kata class; the cluster default unless set. Advised: a runner parses untrusted repository content and runs what the model asks. |
| config.transcriptRetention | string | `""` | How long a review's full model transcript is kept, at least 24h; 720h unless set. |
| config.webUrl | required | `""` | Public URL the dashboard is reached at, e.g. https://kritika.example.com; the webhooks share it under `/hooks/<app name>`. Must be an absolute http(s) URL with no query or fragment. GitHub delivers webhooks to it and sign-in redirects back to it, so the chart's Ingress or HTTPRoute must route this name. |
| configFile | optional | `{}` | The configuration file, as YAML: what is reviewed and how, from `auth` and `apps` to `repositories` and `accounts`. Passed through verbatim, not tpl'd. See docs/configuration.md. |
| database.app.existingSecret | required | `""` | Secret holding the application role's connection URI. |
| database.app.key | string | `"uri"` | Key in that Secret. |
| database.app.role | string | `"kritika_app"` | Name of the application role, asserted at startup (not superuser, no BYPASSRLS, owns nothing). |
| database.owner.existingSecret | required | `""` | Secret holding the owner role's connection URI, used only by the leader for migrations and configuration sync. |
| database.owner.key | string | `"uri"` | Key in that Secret. |
| database.runner.existingSecret | required | `""` | Secret holding the runner role's connection URI; referenced by runner Jobs, never read by kritika serve. |
| database.runner.key | string | `"uri"` | Key in that Secret. |
| database.runner.role | string | `"kritika_runner"` | Name of the runner role, granted only what runner Jobs need. |
| deploymentAnnotations | object | `{}` | Annotations added to the Deployment (e.g. `reloader.stakater.com/auto: "true"`). Pod-level annotations go in `podAnnotations`. |
| env | object | `{}` | Environment variables of the kritika serve container, keyed by name: a plain value, or a map with the variable's `valueFrom` (a Secret, a ConfigMap or a field). Rendered through `tpl`. |
| envFrom | list | `[]` | Secrets and ConfigMaps loaded as environment variables in bulk, each key a variable: the Kubernetes `envFrom` list. A Secret whose keys are the variable names the configuration file references sets them all at once. |
| existingConfigMap | string | `""` | Existing ConfigMap holding the file under the `config.yaml` key; takes precedence over `configFile`. A change to it takes a restart. |
| fullnameOverride | string | `""` | Override the full release name. |
| httpRoute.additionalRules | list | `[]` | Custom rules prepended before the default rule (templated). |
| httpRoute.annotations | object | `{}` | HTTPRoute annotations. |
| httpRoute.apiVersion | string | `""` | HTTPRoute apiVersion; empty defaults to gateway.networking.k8s.io/v1. |
| httpRoute.enabled | bool | `false` | Expose the public Service through a Gateway API HTTPRoute (alternative to ingress). |
| httpRoute.filters | list | `[]` | Filters applied to the default rule. |
| httpRoute.hostnames | list | `[]` | Hostnames matched against the Host header; `config.webUrl`'s host (templated). |
| httpRoute.httpsRedirect | bool | `false` | Redirect HTTP to HTTPS (301) instead of routing to the backend (needs HTTP+HTTPS listeners). |
| httpRoute.kind | string | `""` | HTTPRoute kind; empty defaults to HTTPRoute. |
| httpRoute.labels | object | `{}` | HTTPRoute labels. |
| httpRoute.matches | list | `[{"path":{"type":"PathPrefix","value":"/"}}]` | Match conditions for the default rule. |
| httpRoute.parentRefs | list | `[]` | Gateways (and listeners) this route attaches to. |
| image.digest | string | `""` | Pin the image by digest (sha256:…); when set, overrides the tag. The release pipeline fills it with the published image's digest. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/home-operations/kritika"` | Image repository. |
| image.tag | string | `""` | Overrides the image tag; defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries, for the kritika serve pods and, through the runner ServiceAccount, runner Jobs and the tool images they mount. |
| ingress.annotations | object | `{}` | Ingress annotations. |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Expose the public Service through an Ingress. |
| ingress.hosts | list | `[{"host":"kritika.example.com","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress hosts and their paths; the host is `config.webUrl`'s. |
| ingress.tls | list | `[]` | Ingress TLS configuration. |
| livenessProbe | object | `{"httpGet":{"path":"/healthz","port":"metrics"},"periodSeconds":20}` | Liveness probe, on the metrics port. |
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
| networkPolicy.postgresPort | int | `5432` | Postgres port allowed for egress. |
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
| runner.image | string | `""` | Image for runner Jobs; empty uses the release's `-tools` image when `toolsImage.enabled`, else the chart's image. |
| runner.resources | object | `{}` | Resources for runner pods (KRITIKA_RUNNER_RESOURCES), copied into the pod spec. |
| runner.serviceAccount.annotations | object | `{}` | Annotations for the runner ServiceAccount. |
| runner.serviceAccount.create | bool | `true` | Create the runner ServiceAccount: no permissions, no token mounted, and the chart's `imagePullSecrets` so runner Jobs can pull from a private registry. |
| runner.serviceAccount.name | string | `""` | Runner ServiceAccount name; generated from the release name if empty. |
| runner.tools | list | `[]` | Command-line tools a runner pod mounts from an image for the agent's run tool (KRITIKA_RUNNER_TOOLS), each a `name`, a digest-pinned `image`, the `path` of its binaries and the `commands` it provides. Needs Kubernetes 1.33 or newer, which mounts an image volume with a subPath; the chart refuses to render them on an older cluster. |
| runner.toolsImage.digest | string | `""` | Pin the `-tools` image by digest (sha256:…); when set, overrides the tag. The release pipeline fills it with the published `-tools` image's digest. |
| runner.toolsImage.enabled | bool | `false` | Run runner Jobs on the release's `-tools` image, which adds curl, fd, gh, jq, rg and yq for an agentic review's `agent.commands`: `image.repository` at the chart's tag with `-tools` appended, or pinned by `digest`. |
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

---

_This README is generated by [helm-docs](https://github.com/norwoodj/helm-docs) from `Chart.yaml` and `values.yaml`. Edit those (or `README.md.gotmpl`) and run `mise run generate`._
