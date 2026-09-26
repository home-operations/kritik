# Metrics

The management port serves `/metrics` alongside `/healthz` and `/readyz`.
Beyond the Go runtime, every series is prefixed `kritik_` and labelled by
tenant, installation or model, never by pull request or commit:

| Series                                                                                 | Labels                | What it counts                                                                |
| -------------------------------------------------------------------------------------- | --------------------- | ----------------------------------------------------------------------------- |
| `kritik_webhooks_total`                                                                | installation, outcome | deliveries: enqueued, skipped, ignored, ping, unauthorized, unparsable, error |
| `kritik_polls_total`, `kritik_polled_pull_requests_total`                              | installation, outcome | backstop polls and the open pull requests they handed to ingest               |
| `kritik_reviews_total`, `kritik_review_duration_seconds`                               | tenant, status        | reviews by terminal status and wall time                                      |
| `kritik_findings_total`                                                                | tenant, severity      | findings posted                                                               |
| `kritik_followups_total`                                                               | tenant, outcome       | mentions handled: answered, limited, ignored, failed                          |
| `kritik_context_chunks_total`                                                          | tenant, stage         | context chunks put in front of the model                                      |
| `kritik_index_runs_total`, `kritik_index_chunks_total`                                 | tenant, mode, status  | index runs and chunks embedded                                                |
| `kritik_runner_runs_total`, `kritik_runner_duration_seconds`                           | tenant, kind, outcome | runner Jobs: success, failed, deadline                                        |
| `kritik_lease_wait_seconds`                                                            | tenant, model         | time waiting for a model concurrency slot                                     |
| `kritik_review_snoozes_total`                                                          | tenant, model         | reviews put back on the queue because every model slot was held               |
| `kritik_model_calls_total`, `kritik_model_tokens_total`, `kritik_model_cost_usd_total` | tenant, model, role   | calls; tokens by direction (input, cached, output); provider-reported cost    |
| `kritik_config_drift`                                                                  |                       | 1 while this replica's file differs from the applied one                      |
