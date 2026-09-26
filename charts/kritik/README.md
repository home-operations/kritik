# kritik

![Version](https://img.shields.io/static/v1?label=Version&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->
![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)
![AppVersion](https://img.shields.io/static/v1?label=AppVersion&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->

Multi-tenant AI pull request reviewer for GitHub organisations, backed by Postgres and per-review Kubernetes Jobs

**Homepage:** <https://github.com/home-operations/kritik>

## Usage

kritik ships as an OCI Helm chart. It needs a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) and pgvector, three
roles, the configuration file, and the secrets the file references:

```sh
helm install kritik oci://ghcr.io/home-operations/charts/kritik \
  --set database.app.existingSecret=kritik-postgres-app \
  --set database.owner.existingSecret=kritik-postgres-credentials \
  --set database.runner.existingSecret=kritik-postgres-runner \
  --values my-values.yaml
```

where `my-values.yaml` carries `config.file` (the declarative configuration:
providers, defaults, tenants with installations and repositories) and
`secretMounts` for the GitHub App keys, webhook secrets and provider API
keys the file references by path. Set `embedding.model` and a key to turn
on the vector index and the similar-code context stage.

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

Worker-capable pods serve a forward proxy on `gateway.port`, and runner Jobs
are handed it as `HTTPS_PROXY` and `HTTP_PROXY`. With `networkPolicy.enabled`,
a runner pod can then reach nothing but DNS, Postgres and that port: its git
fetch and every command it runs go through the gateway, which allows a
destination by hostname only. The forges of the file's installations are
always allowed; `egress.allowHosts` in the configuration file adds the rest
(registries, release APIs), and `egress.credentials` names hosts the gateway
adds a bearer token to when a runner sends it a plain `http://` request, so
the runner never holds the token:

```yaml
config:
  file:
    egress:
      allowHosts:
        - api.github.com
        - "*.githubusercontent.com"
        - ghcr.io
      credentials:
        api.github.com: { file: /var/run/secrets/kritik/github-token }
```

The same port is an agentic runner's model endpoint. The worker mints a
token for each run, good for that run until its Job's deadline and revoked
when it ends, and hands it to the pod in place of a provider key; the
gateway answers each step through the tenant's provider with the key only
the worker holds, refuses a step once the run's token budget or the
tenant's `tokensPerMonth` is spent, and records the step's usage. Provider
endpoints are therefore not in a runner's allowlist.

`gateway.enabled: false` removes the listener and the Service and gives runner
pods the `networkPolicy.egressPorts` to anywhere instead. Agentic reviews are
refused without the gateway.

### Runner tools

An agentic repository's `agent.commands` lets the model run allowlisted
binaries over a checkout of the head commit: to read a dependency bump's
release notes and compare view with `curl`, or search with `rg` and `fd`.
The chart's image has none of them, so no command is offered; point
`runner.image` at the release's `-tools` tag, an Alpine image with all
three. Every host `curl` reaches must pass the gateway, so add the
release APIs and registries to `egress.allowHosts` (a GitHub
installation already allows `github.com`):

```yaml
runner:
  image: ghcr.io/home-operations/kritik:<version>-tools
config:
  file:
    egress:
      allowHosts: [api.github.com, "*.githubusercontent.com"]
    tenants:
      - slug: example
        repositories:
          - name: example/home-ops
            mode: agentic
            agent: { commands: [curl, fd, rg] }
```

A command runs without a shell, with an environment of `PATH`, its own
`HOME` and the gateway, and the runner makes itself unreadable to it first,
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

`roles.all` runs everything in one Deployment and is the usual shape. For a
split, disable it and enable `roles.ingest` (webhooks) and `roles.worker`
(queues, runner Jobs, leader duties) with their own replica counts. Any
number of replicas may run; one holds the leader lock at a time.

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
| config.existingConfigMap | string | `""` | Existing ConfigMap holding the file under the `config.yaml` key; takes precedence over `file`. |
| config.extraEnv | list | `[]` | Extra raw env vars merged into every role's container (advanced). |
| config.file | required unless `existingConfigMap` is set | `{}` | The configuration file, as YAML. Passed through verbatim, not tpl'd. See the README for the schema. |
| config.indexWorkers | int | `1` | Index jobs one worker replica runs at once (KRITIK_INDEX_WORKERS), rate-limited apart from reviews. |
| config.logFormat | string | `"json"` | Log format: json or text. |
| config.logLevel | string | `"info"` | Log level: debug, info, warn or error. |
| config.onboardWindow | int | `4` | Onboarding index jobs the leader keeps queued or running at once (KRITIK_ONBOARD_WINDOW); tenants take turns and the most active repositories go first. |
| config.pollInterval | string | `"10m"` | How often the leader lists each installation's open pull requests as a backstop for missed webhooks (Go duration); "0" disables. |
| config.pollLookback | string | `"24h"` | How far back a first or long-idle poll looks (Go duration). |
| config.reloadInterval | string | `"10s"` | How often each replica re-reads the file (Go duration). |
| config.reviewWorkers | int | `2` | Review jobs one worker replica runs at once (KRITIK_REVIEW_WORKERS); follow-ups share the count. A review or index job holds at most one runner pod, so runner pods never exceed the replicas working jobs × (reviewWorkers + indexWorkers). |
| dashboard.keySecret.key | string | `"key"` | Key in that Secret. |
| dashboard.keySecret.name | string | `""` | Secret holding the key that seals dashboard tenants' credentials (`openssl rand -base64 32`); rotate via oldKeysSecret, as losing it makes those credentials unreadable and the pod fail to start. |
| dashboard.oldKeysSecret.key | string | `"old-keys"` | Key in that Secret. |
| dashboard.oldKeysSecret.name | optional | `""` | Secret holding retired sealing keys, comma-separated, only to open values sealed under them. |
| database.app.existingSecret | required | `""` | Secret holding the application role's connection URI. |
| database.app.key | string | `"uri"` | Key in that Secret. |
| database.app.role | string | `"kritik_app"` | Name of the application role, asserted at startup (not superuser, no BYPASSRLS, owns nothing). |
| database.owner.existingSecret | required for roles.all / roles.worker | `""` | Secret holding the owner role's connection URI, used only by the leader for migrations and configuration sync. |
| database.owner.key | string | `"uri"` | Key in that Secret. |
| database.runner.existingSecret | required for roles.all / roles.worker | `""` | Secret holding the runner role's connection URI; referenced by runner Jobs, never read by the worker. |
| database.runner.key | string | `"uri"` | Key in that Secret. |
| database.runner.role | string | `"kritik_runner"` | Name of the runner role, granted only what runner Jobs need. |
| deploymentAnnotations | object | `{}` | Annotations added to every Deployment (e.g. `reloader.stakater.com/auto: "true"`). Pod-level annotations go in `podAnnotations`. |
| embedding.apiKey | string | `""` | API key, rendered into a chart-managed Secret. Prefer `existingSecret`. |
| embedding.baseUrl | string | `"https://openrouter.ai/api/v1"` | OpenAI-compatible embeddings endpoint (OpenRouter serves Voyage's code models). |
| embedding.dims | int | `1024` | Vector dimension, at most 4000. |
| embedding.existingSecret | string | `""` | Existing Secret holding the API key. |
| embedding.existingSecretKey | string | `"api-key"` | Key in that Secret. |
| embedding.maxBatch | int | `64` | Max inputs per embedding request. |
| embedding.maxBatchChars | int | `200000` | Max characters per embedding request. |
| embedding.maxItemChars | int | `16000` | Max characters per input; longer inputs are truncated. |
| embedding.model | string | `""` | Embedding model id; empty disables indexing. |
| embedding.reindexOnModelChange | bool | `false` | Rebuild the index when the model or dimension changes instead of refusing to start. |
| fullnameOverride | string | `""` | Override the full release name. |
| gateway.enabled | bool | `true` | Serve the gateway on `all` and `worker` pods: the forward proxy runner Jobs are handed as `HTTPS_PROXY`, allowing only the hosts the configuration file names (forges, `egress.allowHosts`), so runner pods need no direct internet egress (ADR-0008), and the model endpoint an agentic runner calls with a per-run token, so no provider key enters a runner pod (ADR-0004). Agentic reviews are refused without it. |
| gateway.port | int | `8082` | Gateway port on the pods and its Service. |
| httpRoute.annotations | object | `{}` | HTTPRoute annotations. |
| httpRoute.apiVersion | string | `""` | HTTPRoute apiVersion; empty defaults to gateway.networking.k8s.io/v1. |
| httpRoute.enabled | bool | `false` | Expose the webhook listener via a Gateway API HTTPRoute. |
| httpRoute.hostnames | list | `[]` | Hostnames matched against the Host header (templated). |
| httpRoute.labels | object | `{}` | HTTPRoute labels. |
| httpRoute.matches | list | `[{"path":{"type":"PathPrefix","value":"/hooks"}}]` | Match conditions for the route. |
| httpRoute.parentRefs | list | `[]` | Gateways (and listeners) this route attaches to. |
| httpRoute.web.annotations | object | `{}` | HTTPRoute annotations. |
| httpRoute.web.apiVersion | string | `""` | HTTPRoute apiVersion; empty defaults to gateway.networking.k8s.io/v1. |
| httpRoute.web.enabled | bool | `false` | Expose the dashboard via a Gateway API HTTPRoute. |
| httpRoute.web.hostnames | list | `[]` | Hostnames matched against the Host header (templated). |
| httpRoute.web.labels | object | `{}` | HTTPRoute labels. |
| httpRoute.web.matches | list | `[{"path":{"type":"PathPrefix","value":"/"}}]` | Match conditions for the route. |
| httpRoute.web.parentRefs | list | `[]` | Gateways (and listeners) this route attaches to. |
| image.digest | string | `""` | Pin the image by digest (sha256:…); when set, overrides the tag. The release pipeline fills it with the published image's digest. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/home-operations/kritik"` | Image repository. |
| image.tag | string | `""` | Overrides the image tag; defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries. |
| ingress.annotations | object | `{}` | Ingress annotations. |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Expose the webhook listener via an Ingress. |
| ingress.hosts | list | `[{"host":"kritik.example.com","paths":[{"path":"/hooks","pathType":"Prefix"}]}]` | Ingress hosts and their paths. |
| ingress.tls | list | `[]` | Ingress TLS configuration. |
| ingress.web.annotations | object | `{}` | Ingress annotations. |
| ingress.web.className | string | `""` | IngressClass name. |
| ingress.web.enabled | bool | `false` | Expose the dashboard via an Ingress. |
| ingress.web.hosts | list | `[{"host":"dash.example.com","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress hosts and their paths. |
| ingress.web.tls | list | `[]` | Ingress TLS configuration. |
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
| podDisruptionBudget.enabled | bool | `false` | Create a PodDisruptionBudget per role with more than one replica. |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum pods of a role that may be unavailable, as a count or percentage. @schema type: [integer, string] @schema |
| podLabels | object | `{}` | Labels added to the pods. |
| podSecurityContext | object | `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level securityContext (non-root uid/gid 65532, RuntimeDefault seccomp). |
| priorityClassName | string | `""` | PriorityClass for the pods. Empty uses the cluster default. |
| rbac.create | bool | `true` | Create the Role and RoleBinding the worker needs: Jobs in the release namespace, their pods and logs, and the Secrets it hands them. Nothing cluster-wide. |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"metrics"},"periodSeconds":10}` | Readiness probe, on the metrics port. A replica is ready once it has a database connection and its listeners are up. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}` | Pod resource requests/limits shared by every role; `roles.<role>.resources` overrides per role. |
| roles.all.enabled | bool | `true` | Run the single-process topology: webhooks, leader duties and the worker in one Deployment. |
| roles.all.replicas | int | `1` | Replicas. Any replica can serve webhooks and work jobs; exactly one holds the leader lock at a time. |
| roles.all.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| roles.ingest.enabled | bool | `false` | Run webhook ingest as its own Deployment (split topology). |
| roles.ingest.replicas | int | `2` | Replicas for the ingest Deployment. |
| roles.ingest.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| roles.web.enabled | bool | `false` | Run the dashboard as its own Deployment (split topology). Requires `web.url` to be set. |
| roles.web.replicas | int | `1` | Replicas for the web Deployment. |
| roles.web.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| roles.worker.enabled | bool | `false` | Run the worker (queues, runner Jobs, leader duties) as its own Deployment (split topology). |
| roles.worker.replicas | int | `1` | Replicas for the worker Deployment. |
| roles.worker.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| runner.deadline | string | `"15m"` | Default active deadline for a runner Job (Go duration); tenants may lower it in the file. |
| runner.image | string | `""` | Image for runner Jobs; empty uses the chart's image. The release's `-tools` tag (e.g. `ghcr.io/home-operations/kritik:1.2.3-tools`) adds curl, fd and rg for an agentic review's `agent.commands`. |
| runner.runtimeClassName | string | `""` | RuntimeClass for runner Jobs (e.g. `gvisor`, `kata`). Advised: a runner parses untrusted repository content and, in agentic mode, runs what the model asks; a sandboxed runtime keeps it from the node's kernel. Empty uses the cluster default. |
| runner.serviceAccount.annotations | object | `{}` | Annotations for the runner ServiceAccount. |
| runner.serviceAccount.create | bool | `true` | Create the runner ServiceAccount (no permissions, no token mounted). |
| runner.serviceAccount.name | string | `""` | Runner ServiceAccount name; generated from the release name if empty. |
| runner.ttl | string | `"1h"` | How long a finished Job stays for kubectl before Kubernetes removes it (Go duration); the run row keeps everything the Job knew. |
| secretMounts | list | `[]` | Secrets mounted as files for the configuration file to reference. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container securityContext (no privilege escalation, read-only root filesystem, drops ALL capabilities). |
| service.metricsPort | int | `8081` | Metrics and probe port, served by every pod. |
| service.port | int | `8080` | Webhook port (`POST /hooks/{installation}`), served by `all` and `ingest` pods. |
| service.type | string | `"ClusterIP"` | Service type for the webhook listener. |
| service.webPort | int | `8083` | Dashboard port, served by `all` (once `web.url` is set) and `web` pods. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| serviceAccount.automount | bool | `true` | Automount the API token. The worker needs it to create runner Jobs; a pure ingest topology could turn it off. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount the roles run as. |
| serviceAccount.name | string | `""` | ServiceAccount name; generated from the release name if empty. |
| terminationGracePeriodSeconds | int | `150` | Grace period for a clean shutdown: the worker stops taking jobs and lets running ones finish for up to 30s, and its gateway lets model steps in flight finish for up to 2m. |
| tolerations | list | `[]` | Tolerations for pod scheduling. |
| volumeMounts | list | `[]` | Additional volume mounts on every container. |
| volumes | list | `[]` | Additional volumes on every Deployment. |
| web.port | int | `8083` | Dashboard port, served by `all` (once `web.url` is set) and `web` pods. |
| web.url | required for roles.web | `""` | Public URL the dashboard is reached at, e.g. https://kritik.example.com. Must be an absolute http(s) URL with no query or fragment. |

---

_This README is generated by [helm-docs](https://github.com/norwoodj/helm-docs) from `Chart.yaml` and `values.yaml`. Edit those (or `README.md.gotmpl`) and run `mise run generate`._
