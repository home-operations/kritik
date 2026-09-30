# ADR-0024: one server process, `kritik serve` and `kritik run`

- **Status:** Proposed
- **Date:** 2026-09-30
- **Authors:** onedr0p.
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.3 (the
  `--role` flag and its `all`, `ingest`, `worker` and `runner` roles) and
  [ADR-0009](0009-web-dashboard.md) §2.1 (the `web` role).

> Scope: how kritik's processes are divided and started, and how a
> restart treats the jobs it cuts. What each part does is unchanged.

## 1. Context

One binary takes `--role`: `all` runs everything, `ingest` receives
webhooks, `worker` works the queues, creates runner Jobs, serves the
gateway and can lead, `web` serves the dashboard, and `runner` is one
run in a Job. The split roles were meant for a multi-tenant service that
scales each part apart (ADR-0002). Since ADR-0014 kritik is a
self-hosted instance for the accounts one GitHub App serves: the heavy
work already scales out as runner Jobs, and throughput is bound by model
limits, not by the process layout. The split still costs four
Deployment shapes, their Services, network policy rules and tests, and
two public ports so that webhooks and the dashboard could be routed to
different Deployments. The one thing it buys, a dashboard without the
owner database role, is lost in `all`, the default.

Every configuration change is now a rollout (ADR-0022). A stopping
process cancels every running job at once: the River client's start
context is the signal context, so the drain the worker's shutdown waits
for never happens, and a review cut that way is marked failed.

## 2. Decision

### 2.1 Two commands

`kritik serve` is the service: webhooks, the dashboard and its API, the
job queues, runner Jobs, the gateway and, on the replica holding the
leader lock, the leader duties. `kritik run` is one review or index run
in a runner Job, as `--role runner` was. `--role` and the `ingest`,
`worker` and `web` roles go.

The chart runs one Deployment of `serve`, two replicas by default
(ADR-0022 §2.3), with top-level `replicas` and `resources` in place of
`roles.*`.

### 2.2 One public port

Webhooks (`/hooks/<app>`) and the dashboard share one listener and one
Service, so the chart's Ingress or HTTPRoute has one backend. The
gateway keeps its own port, which only runner pods reach, and health
and metrics theirs.

### 2.3 A restart drains, and retries what it cuts

A stopping `serve` stops taking jobs and lets running ones finish for a
drain window inside the pod's grace period. A review or index job still
running when the window ends is cancelled and handed back to the queue
to retry, on the other replica or after the restart, rather than
marked failed. A job that ends by its own timeout, or that an admin
cancels, ends as before.

### 2.4 Where a split would come back

The gateway is the one part that takes requests from code a model chose
to run, and it shares a process with every secret `serve` holds. If the
threat model tightens, it is the part to move into its own Deployment,
holding only the provider keys and egress credentials it uses. Sandboxed
runners, per-run tokens and the host allowlist make that unnecessary
today.

## 3. Consequences

- One Deployment, one public Service and one command to run, and the
  role names stop being a question.
- The dashboard runs in the process that holds the owner database role,
  as it did in `all`.
- A rollout finishes most running reviews and retries the rest; a review
  cut after the drain window runs again from the start, and its first
  run's model calls are paid twice.
- Deployments using the split roles, `--role` or the dashboard port must
  move to `serve` and one port once.

## 4. Rejected alternatives

- **Keep the split, renamed.** Better names for parts nobody scales
  apart.
- **Resume a cut review's runner Job on retry.** It would save the
  repeated model calls, at the cost of attaching a new worker to a run
  another one started; the drain window already lets most reviews
  finish.
