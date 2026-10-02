# Metrics

The management port serves `/metrics` alongside `/healthz` and `/readyz`.
Beyond the Go runtime, every series is prefixed `kritika_` and labelled by
account, connection or model, never by pull request or commit:

| Series                                                                                    | Labels                 | What it counts                                                                                                                                                                                                        |
| ----------------------------------------------------------------------------------------- | ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kritika_webhooks_total`                                                                  | connection, outcome    | deliveries: enqueued, recorded, skipped, ignored, ping, unsigned (the App has no webhook secret), unauthorized, unparsable, too_large, unreadable, unknown_connection (with no connection), undeclared_account, error |
| `kritika_polls_total`, `kritika_polled_pull_requests_total`                               | connection, outcome    | backstop polls and the open pull requests they handed to ingest                                                                                                                                                       |
| `kritika_reviews_total`, `kritika_review_duration_seconds`                                | account, status        | reviews by terminal status and wall time                                                                                                                                                                              |
| `kritika_findings_total`                                                                  | account, severity, category | findings posted                                                                                                                                                                                                  |
| `kritika_followups_total`                                                                 | account, outcome       | mentions handled: answered, limited, ignored, failed                                                                                                                                                                  |
| `kritika_index_runs_total`, `kritika_index_chunks_total`                                  | account, mode, status  | index runs and chunks embedded                                                                                                                                                                                        |
| `kritika_context_chunks_total`                                                            | account, stage         | context chunks a review's prompt was given, by stage: overlay, definition, caller, similar (the index)                                                                                                                |
| `kritika_runner_runs_total`, `kritika_runner_duration_seconds`                            | account, kind, outcome | runner Jobs: success, failed, deadline                                                                                                                                                                                |
| `kritika_lease_wait_seconds`                                                              | account, model         | time waiting for a model concurrency slot                                                                                                                                                                             |
| `kritika_review_snoozes_total`                                                            | account, model         | reviews put back on the queue because every model slot was held                                                                                                                                                       |
| `kritika_jobs_rescued_total`                                                              | kind, state            | jobs a replica died working, handed back to the queue by the leader: retryable, discarded (no attempts left), cancelled (a cancel was asked)                                                                          |
| `kritika_model_calls_total`, `kritika_model_tokens_total`, `kritika_model_cost_usd_total` | account, model, role   | calls; tokens by direction (input, cached, output); provider-reported cost; `model` is the one that answered, so OpenRouter's fallback shows under its own                                                            |
| `kritika_forge_rate_limits_total`                                                      | connection, outcome    | responses GitHub refused for a rate limit: waited (the request waited the limit out and was sent again) or refused (the wait was over a minute, or the request could not be resent)                                |
| `kritika_egress_requests_total`                                                           | kind, outcome          | requests runner pods made through the gateway: connect or http; allowed, refused, error                                                                                                                               |
| `kritika_transcript_writes_total`                                                         | kind, outcome          | model calls recorded for the transcript view: agent_step or followup; ok, error                                                                                                                                       |
| `kritika_db_pool_connections`, `kritika_db_pool_max_connections`                          | pool, state            | connections the app and owner pools hold, by state (idle, acquired, constructing), and each pool's ceiling; a pool at its ceiling queues jobs and requests                                                       |
| `kritika_db_pool_acquires_total`, `kritika_db_pool_empty_acquires_total`, `kritika_db_pool_acquire_seconds_total` | pool | connections taken from a pool, how many had to wait for one to free, and the time spent waiting                                                                                                                 |
| `kritika_config_drift`                                                                    |                        | 1 while this replica's file differs from the applied one                                                                                                                                                              |
| `kritika_config_error`                                                                    |                        | 1 while the leader's latest attempt to apply the configuration to the store was refused; the last applied configuration stays live meanwhile                                                                      |
| `kritika_leader`                                                                          |                        | 1 while this replica holds the leader lock                                                                                                                                                                            |

## Alerts

The chart's `monitoring.prometheusRule.enabled` creates a Prometheus Operator
`PrometheusRule` with the alerts below, grouped by the metrics Service's
`namespace` and `job`. Each one's wait is a value
(`monitoring.prometheusRule.*For`), and `additionalRuleLabels` adds a
routing label to every rule.

| Alert                    | Fires when                                                           | Severity | Wait |
| ------------------------ | -------------------------------------------------------------------- | -------- | ---- |
| `KritikaNoLeader`        | `sum(kritika_leader) == 0`: no replica holds the leader lock          | critical | 5m   |
| `KritikaMultipleLeaders` | `sum(kritika_leader) > 1`: more than one replica reports holding it   | critical | 2m   |
| `KritikaConfigError`     | `kritika_config_error == 1`: the leader could not apply the file      | warning  | 5m   |
| `KritikaConfigDrift`     | `kritika_config_drift == 1`: a replica's file differs from the applied one | warning | 15m |

Without a leader nothing migrates, applies configuration, polls or rescues
the jobs a dead replica left; a standby takes the lock within one leader
retry interval once its owner connection reaches the database, and Postgres
drops a dead leader's session, and with it the lock, within about a minute.
Two leaders can show for one retry interval through a database failover,
while the old leader's lock connection has not yet failed; longer than that
means the replicas are not on one database. Drift is expected for the
minute or two a rollout takes, until the new pod leads and applies its
file.

## Grafana dashboard

The chart's `monitoring.dashboards.enabled` renders a Grafana dashboard of
the series above (`charts/kritika/dashboards/kritika.json`) as a ConfigMap
labelled `grafana_dashboard: "1"` for the kube-prometheus-stack sidecar, or,
with `monitoring.dashboards.grafanaOperator.enabled`, as a `GrafanaDashboard`
for grafana-operator that imports the ConfigMap into the Grafana instance
`grafanaOperator.matchLabels` selects. It is filtered by the metrics
Service's `namespace` and `job`, then by `account` and `model`, and its rows
follow a review's path: health (leaders, configuration, drift, rescued
jobs), intake (webhooks, backstop polls, rate limits), reviews (status,
duration, findings, follow-ups, the wait for a model slot), models (calls,
tokens, cost per hour and over the time range), runner Jobs and the index,
and the database pools.
