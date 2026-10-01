# Helm chart values

The **kritika** chart is documented value by value in the generated
[chart README](https://github.com/home-operations/kritika/blob/main/charts/kritika/README.md),
kept in step with `values.yaml` by helm-docs (CI fails if it goes stale). The
chart also ships a
[`values.schema.json`](https://github.com/home-operations/kritika/blob/main/charts/kritika/values.schema.json)
for editor completion and `helm install` validation. The chart README also
covers the egress gateway, runner tools and the runner sandbox; the database
has its own page, [Postgres with CloudNativePG](database.md).

This page is the orientation: which groups of values exist and where their
behavior is explained, followed by the full `values.yaml`.

- **`image`**, **`replicas`**, **`strategy`**, **`resources`** and the pod
  settings: the one Deployment of `kritika serve`.
- **`web`**: the public URL the dashboard and GitHub's webhooks share.
- **`config`** and **`existingConfigMap`**: the configuration file, inline or
  from an existing ConfigMap. See [Configuration file](configuration.md).
- **`env`** and **`envFrom`**: every other `KRITIKA_*` variable by name (sign-in,
  polling, retention, workers, the runner Jobs' deadline and RuntimeClass) and
  the Secrets that set the variables the configuration file names. See
  [Configuration file](configuration.md).
- **`logging`**: log level and format.
- **`database`**: the Postgres host and the owner, application and runner
  roles' Secrets.
- **`runner`**: the image, TTL, resources and tools of the runner Jobs.
- **`service`**, **`ingress`** and **`httpRoute`**: the public, metrics and
  gateway ports and how the public one is exposed. The gateway is the egress
  proxy runner pods reach the outside and their model through.
- **`networkPolicy`**: the policies that confine runner pods to the gateway.
- **`monitoring`** and the probes: see [Metrics](metrics.md).

## values.yaml

```yaml
--8<-- "charts/kritika/values.yaml"
```
