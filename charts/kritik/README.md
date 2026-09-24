# kritik

![Version](https://img.shields.io/static/v1?label=Version&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->
![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)
![AppVersion](https://img.shields.io/static/v1?label=AppVersion&message=0.0.0&color=informational&style=flat-square) <!-- x-release-please-version -->

Multi-tenant AI pull request reviewer for GitHub organisations, backed by Postgres and per-review Kubernetes Jobs

**Homepage:** <https://github.com/home-operations/kritik>

## Usage

kritik ships as an OCI Helm chart. It needs a pgvector-enabled Postgres with
three roles, the configuration file, and the secrets the file references:

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
on the pgvector index and the similar-code context stage.

### Database

kritik separates three Postgres roles and refuses to start otherwise: the
**owner** (runs migrations and applies configuration on the leader, must not
be a superuser), the **application** role (`database.app.role`, must not own
the tables so row-level security applies to it) and the **runner** role
(`database.runner.role`, handed to runner Jobs, can only write its own run).
The `vector` extension must exist before the first start.

On CloudNativePG, the bootstrap owner is `owner`, the other two are declared
under `spec.managed.roles`, and a `Database` resource creates the extension:

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
```

Each `passwordSecret` is a basic-auth Secret; kritik reads a `uri` key from
the Secrets named in `database.*.existingSecret`, so either use CNPG's
generated `uri` for the owner or add one for the managed roles (an External
Secrets `Password` generator plus a templated `uri` works).

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
| config.pollInterval | string | `"10m"` | How often the leader lists each installation's open pull requests as a backstop for missed webhooks (Go duration); "0" disables. |
| config.pollLookback | string | `"24h"` | How far back a first or long-idle poll looks (Go duration). |
| config.reloadInterval | string | `"10s"` | How often each replica re-reads the file (Go duration). |
| config.reviewWorkers | int | `2` | Review jobs one worker replica runs at once (KRITIK_REVIEW_WORKERS); follow-ups share the count. |
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
| embedding.dims | int | `1024` | Vector dimension, at most 4000 (the halfvec index limit). |
| embedding.existingSecret | string | `""` | Existing Secret holding the API key. |
| embedding.existingSecretKey | string | `"api-key"` | Key in that Secret. |
| embedding.maxBatch | int | `64` | Max inputs per embedding request. |
| embedding.maxBatchChars | int | `200000` | Max characters per embedding request. |
| embedding.maxItemChars | int | `16000` | Max characters per input; longer inputs are truncated. |
| embedding.model | string | `""` | Embedding model id; empty disables indexing. |
| embedding.reindexOnModelChange | bool | `false` | Rebuild the index when the model or dimension changes instead of refusing to start. |
| fullnameOverride | string | `""` | Override the full release name. |
| httpRoute.annotations | object | `{}` | HTTPRoute annotations. |
| httpRoute.apiVersion | string | `""` | HTTPRoute apiVersion; empty defaults to gateway.networking.k8s.io/v1. |
| httpRoute.enabled | bool | `false` | Expose the webhook listener via a Gateway API HTTPRoute. |
| httpRoute.hostnames | list | `[]` | Hostnames matched against the Host header (templated). |
| httpRoute.labels | object | `{}` | HTTPRoute labels. |
| httpRoute.matches | list | `[{"path":{"type":"PathPrefix","value":"/hooks"}}]` | Match conditions for the route. |
| httpRoute.parentRefs | list | `[]` | Gateways (and listeners) this route attaches to. |
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
| networkPolicy.egressPorts | list | `[443]` | TCP ports the pods may egress to for forges, model endpoints and git remotes. |
| networkPolicy.enabled | bool | `false` | Create the NetworkPolicies. |
| networkPolicy.postgresPort | int | `5432` | Postgres port allowed for egress. |
| nodeSelector | object | `{}` | Node selector for pod scheduling. |
| podAnnotations | object | `{}` | Annotations added to the pods. |
| podDisruptionBudget.enabled | bool | `false` | Create a PodDisruptionBudget per role with more than one replica. |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum pods of a role that may be unavailable, as a count or percentage. @schema type: [integer, string] @schema |
| podLabels | object | `{}` | Labels added to the pods. |
| podSecurityContext | object | `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level securityContext (non-root uid/gid 65532, RuntimeDefault seccomp). |
| priorityClassName | string | `""` | PriorityClass for the pods. Empty uses the cluster default. |
| rbac.create | bool | `true` | Create the Role and RoleBinding the worker needs: Jobs in the release namespace, plus their pods and logs. Nothing cluster-wide. |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"metrics"},"periodSeconds":10}` | Readiness probe, on the metrics port. A replica is ready once it has a database connection and its listeners are up. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}` | Pod resource requests/limits shared by every role; `roles.<role>.resources` overrides per role. |
| roles.all.enabled | bool | `true` | Run the single-process topology: webhooks, leader duties and the worker in one Deployment. |
| roles.all.replicas | int | `1` | Replicas. Any replica can serve webhooks and work jobs; exactly one holds the leader lock at a time. |
| roles.all.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| roles.ingest.enabled | bool | `false` | Run webhook ingest as its own Deployment (split topology). |
| roles.ingest.replicas | int | `2` | Replicas for the ingest Deployment. |
| roles.ingest.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| roles.worker.enabled | bool | `false` | Run the worker (queues, runner Jobs, leader duties) as its own Deployment (split topology). |
| roles.worker.replicas | int | `1` | Replicas for the worker Deployment. |
| roles.worker.resources | object | `{}` | Resources for this role's pods; empty falls back to `resources`. |
| runner.deadline | string | `"15m"` | Default active deadline for a runner Job (Go duration); tenants may lower it in the file. |
| runner.image | string | `""` | Image for runner Jobs; empty uses the chart's image. |
| runner.serviceAccount.annotations | object | `{}` | Annotations for the runner ServiceAccount. |
| runner.serviceAccount.create | bool | `true` | Create the runner ServiceAccount (no permissions, no token mounted). |
| runner.serviceAccount.name | string | `""` | Runner ServiceAccount name; generated from the release name if empty. |
| runner.ttl | string | `"1h"` | How long a finished Job stays for kubectl before Kubernetes removes it (Go duration); the run row keeps everything the Job knew. |
| secretMounts | list | `[]` | Secrets mounted as files for the configuration file to reference. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container securityContext (no privilege escalation, read-only root filesystem, drops ALL capabilities). |
| service.metricsPort | int | `8081` | Metrics and probe port, served by every pod. |
| service.port | int | `8080` | Webhook port (`POST /hooks/{installation}`), served by `all` and `ingest` pods. |
| service.type | string | `"ClusterIP"` | Service type for the webhook listener. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| serviceAccount.automount | bool | `true` | Automount the API token. The worker needs it to create runner Jobs; a pure ingest topology could turn it off. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount the roles run as. |
| serviceAccount.name | string | `""` | ServiceAccount name; generated from the release name if empty. |
| terminationGracePeriodSeconds | int | `45` | Grace period for a clean shutdown; the worker stops taking jobs and lets running ones finish for up to 30s. |
| tolerations | list | `[]` | Tolerations for pod scheduling. |
| volumeMounts | list | `[]` | Additional volume mounts on every container. |
| volumes | list | `[]` | Additional volumes on every Deployment. |

---

_This README is generated by [helm-docs](https://github.com/norwoodj/helm-docs) from `Chart.yaml` and `values.yaml`. Edit those (or `README.md.gotmpl`) and run `mise run generate`._
