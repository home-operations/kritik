# ADR-0022: the configuration is read at startup, secrets come from the environment, and two replicas run

- **Status:** Proposed
- **Date:** 2026-09-30
- **Authors:** onedr0p.
- **Amends:** [ADR-0014](0014-github-app-only-self-hosted.md) §2.2 (a
  secret's `_FILE` form, and a file edit that hot-reloads) and
  [ADR-0019](0019-configuration-in-git.md) §2.1 (the file re-read on an
  interval, and `{ file: path }` secret references).

> Scope: when kritik reads its configuration, where its secrets come
> from, and how many replicas the chart runs. What the configuration
> says is unchanged.

## 1. Context

Every replica re-reads the configuration file on an interval, and
content that does not load leaves the last good configuration running.
That takes a file watcher, a record of the refused content, a
`kritik_config_error{stage="load"}` gauge that can be raised at runtime,
a Configuration page banner for it, and a `reloadInterval` in the chart.
A secret is `{ env: NAME }` or `{ file: path }`, and every secret
variable has a `_FILE` twin, so there are two ways to hand kritik each
secret and the chart mounts Secrets as files for one of them.

The reload saves a restart, but kritik's configuration changes a few
times a week, through git, and a deployment already rolls pods for every
other change. A pod that fails to start on bad configuration never
becomes ready, so a rolling update keeps the old pods serving, which is
what "keep the last good configuration" does by hand. The reload also
cannot pick up a changed secret: the environment is fixed at startup,
and a mounted file's change is not seen unless the file itself changes.

The chart runs one replica by default, so a restart is a gap in which
webhooks fail. Everything replicas share already lives in Postgres:
sessions, sign-in state, gateway tokens, model-slot leases, the leader
lock and live events.

## 2. Decision

### 2.1 The configuration is read once, at startup

kritik reads the file and the `KRITIK_*` variables over it once, when it
starts. A configuration that does not load fails startup, and a change
takes a restart. The watcher, the refused-content record, the `load`
stage of `kritik_config_error`, the setup status's `configError` and
`KRITIK_CONFIG_RELOAD_INTERVAL` go. The leader applies the configuration
to the store when it is elected, as before; a store that refuses it
still raises the `apply` stage.

The chart annotates each pod template with a checksum of the file it
renders, so a changed `config.file` rolls the pods. A file from
`existingConfigMap` is not rendered by the chart: a deployment using one
restarts the pods itself, for example with stakater's Reloader.

### 2.2 Secrets come from the environment

A secret in the file is `{ env: NAME }`; `{ file: path }` goes, and so
does every `_FILE` variable (`KRITIK_AUTH_ADMIN_PASSWORD_FILE`,
`KRITIK_AUTH_OIDC_CLIENT_SECRET_FILE`, `KRITIK_AUTH_GITHUB_CLIENT_SECRET_FILE`,
`KRITIK_APPS_PRIVATE_KEY_FILE`, `KRITIK_APPS_WEBHOOK_SECRET_FILE` and
`KRITIK_PROVIDERS_API_KEY_FILE`). An app's `clientId` is inline or
`{ env: NAME }`. After loading, kritik removes every variable a secret
came from from its own environment, as it already does for the database
URLs, so a later lookup or a child process does not see it; the copy the
kernel shows in `/proc/<pid>/environ` stays.

The chart's `secretMounts` becomes `secretEnv`: each entry sets one
variable from one key of an existing Secret.

### 2.3 Two replicas

The chart runs two replicas of the `all` role by default, with a
PodDisruptionBudget that keeps one available. With two, the rolling
update of §2.1 keeps one pod serving webhooks and the dashboard
throughout. One replica holds the leader lock; both serve webhooks, the
dashboard and the gateway, and work jobs.

## 3. Consequences

- A configuration change is a rollout: in-flight jobs on a replacing pod
  drain for up to its grace period, and the leader lock moves when its
  holder stops.
- A bad configuration shows as a pod that does not become ready and logs
  why, not as a banner on the Configuration page.
- A rotated secret is picked up by the rollout that follows it.
- Each replica keeps its own login rate limit, so an address gets that
  many attempts per replica.
- Every deployment's secret mounts and `_FILE` variables must be moved to
  environment variables once.

## 4. Rejected alternatives

- **Keep the reload and re-read secret files on it.** It would make
  rotation work without a restart, at the cost of keeping the reload
  plumbing, for changes that are rare and already arrive through a
  deployment.
- **Keep `{ file: path }` beside `{ env: NAME }`.** Two ways to hand over
  each secret, and a file reference only helps with a reload.
- **`envFrom` a whole Secret.** A Secret's keys, such as
  `private-key.pem`, are often not valid variable names, and an explicit
  mapping says which variable carries which secret.
