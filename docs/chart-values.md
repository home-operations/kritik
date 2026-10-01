# Helm chart values

The **kritik** chart is documented value by value in the generated
[chart README](https://github.com/home-operations/kritik/blob/main/charts/kritik/README.md),
kept in step with `values.yaml` by helm-docs (CI fails if it goes stale). The
chart also ships a
[`values.schema.json`](https://github.com/home-operations/kritik/blob/main/charts/kritik/values.schema.json)
for editor completion and `helm install` validation. The chart README also
covers the CloudNativePG setup for the three database roles, the egress
gateway, runner tools and the runner sandbox.

This page is the orientation: which groups of values exist and where their
behavior is explained, followed by the full `values.yaml`.

- **`image`**, **`replicas`**, **`strategy`**, **`resources`** and the pod
  settings: the one Deployment of `kritik serve`.
- **`web`**: the public URL the dashboard and GitHub's webhooks share.
- **`auth`**: how people sign in to the dashboard, as a local admin, through
  OIDC or with GitHub. See [Configuration file](configuration.md).
- **`config`**: the configuration file, inline or from an existing ConfigMap,
  and how kritik runs (polling, retention, log level, workers). See
  [Configuration file](configuration.md).
- **`database`** and **`secretEnv`**: the owner, application and runner DSNs,
  and the Secrets that set the environment variables the configuration file
  names.
- **`runner`**: the image, TTL, deadline, resources, tools and RuntimeClass of
  the runner Jobs.
- **`gateway`**: the port of the egress gateway runner pods reach the outside
  and their model through.
- **`service`**, **`ingress`** and **`httpRoute`**: the public and metrics
  ports and how the public one is exposed.
- **`networkPolicy`**: the policies that confine runner pods to the gateway.
- **`monitoring`** and the probes: see [Metrics](metrics.md).

## values.yaml

```yaml
--8<-- "charts/kritik/values.yaml"
```
