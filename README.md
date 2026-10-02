<div align="center">

# kritika

**Repository-aware AI pull request review for GitHub.**

[![CI](https://img.shields.io/github/actions/workflow/status/home-operations/kritika/ci.yaml?branch=main&label=ci)](https://github.com/home-operations/kritika/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/actions/workflow/status/home-operations/kritika/release.yaml?branch=main&label=release)](https://github.com/home-operations/kritika/actions/workflows/release.yaml)
[![License](https://img.shields.io/github/license/home-operations/kritika)](https://github.com/home-operations/kritika/blob/main/LICENSE)

</div>

> [!WARNING]
> kritika is not production ready. It is under active development and has no
> release yet: configuration, the database schema and the APIs change without
> notice, and there is no upgrade path from one commit to the next.

kritika indexes a repository, reviews each pull request against that context,
posts one sticky summary comment plus inline findings and a commit status, and
answers follow-ups when the bot is @-mentioned. A pull request from a fork is
reviewed when a maintainer asks with `@<bot> review`. One deployment serves any
number of forge accounts, and every index and review job runs in its own
Kubernetes Job pod that holds no secrets.

📖 **Docs site: <https://kritika.home-operations.com/>**: setup, the
configuration file, repository settings, chart values, the dashboard,
metrics and development.

## Features

- **Context beyond the diff.** Whole declarations the diff touches,
  definitions of identifiers on changed lines and callers of changed
  declarations, cut by tree-sitter, plus the most similar chunks from a
  VectorChord index of the default branch, and the issues the description
  says the pull request closes, which the review judges the change against.
- **An agent, not one prompt.** Each review is a bounded, read-only tool loop
  over the head commit, optionally with allowlisted commands (`gh`, `curl`,
  `fd`, `jq`, `rg`, `yq`) so it can read a dependency bump's release notes;
  `agent.maxSteps: 1` makes it one call.
- **Fixes you can apply.** A finding offers its fix as a one-click suggestion,
  with a prompt a coding agent can apply it from.
- **Incremental reviews.** A later push is reviewed against what changed since
  the last review, an earlier finding it no longer finds has its thread
  resolved, `settle` folds a burst of force-pushes into one, and
  `maxAutoReviews` pauses a long-lived pull request's automatic reviews,
  as `@<bot> pause` does on request.
- **Follow-ups.** Someone with write access can @-mention the bot and get an
  answer in the thread, or reply `@<bot> dismiss <reason>` in a finding's
  thread to have it resolved and never raised again on that pull request.
- **Approvals, opt-in.** A repository or the instance can have a review that
  finds nothing blocking or important approve the pull request, and a later
  review that does withdraw it.
- **Providers and limits.** OpenRouter, OpenAI and Anthropic adapters, with
  per-account concurrency, daily review and monthly token caps. The provider
  key never enters a runner pod: the agent reaches its model through
  kritika's gateway.
- **Repository overrides.** A `.kritika.yaml`, read from the merge-base, can
  narrow the admin's settings and bring its own rules, context files and
  comment templates.
- **Configuration in git.** One YAML file holds the whole configuration,
  read at startup, and a change rolls the pods; secrets stay in Secrets,
  which reach kritika as environment variables.
- **Dashboard.** Sign-in, live review state, full model transcripts, the
  running configuration, repository on/off and an audit log.

## Installing

kritika ships as an OCI Helm chart, `oci://ghcr.io/home-operations/charts/kritika`.
The chart's [README](charts/kritika/README.md) lists every value and shows the
CloudNativePG setup for the three database roles. In short: a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) (and the pgvector it
builds on) loaded, with an owner, an application and a runner role; the one
public URL under `config.webUrl`, which the dashboard and GitHub's webhooks
share; a way to sign in under `auth`; and the configuration file under
`configFile`.
It runs as one Deployment of `kritika serve`, which creates a runner Job for
each review and index run.

The [setup guide](https://kritika.home-operations.com/setup/) takes a fresh instance through its GitHub
App, model key and embedder to its first review; the dashboard's setup
checklist shows what is still missing.

Security notes:

- Install kritika into a namespace of its own: runner Jobs run in the release
  namespace, and kritika's Role can create, patch and delete every Secret
  there, though it can never get or list one.
- Turn on the chart's NetworkPolicy: runner pods then reach the outside
  only through kritika's egress gateway, a forward proxy that allows
  destinations by hostname (github.com and the configuration's
  `egress.allowHosts`), and never hold the credentials `egress.credentials`
  lets the gateway add. Every review's model calls go through it too.
- Run runner Jobs under a sandboxed RuntimeClass such as gVisor
  (the chart's `config.runnerRuntimeClass`) where the cluster has one, since the pod
  parses untrusted content.

## Documentation

- [Setup](https://kritika.home-operations.com/setup/): from install to the first review, the GitHub App,
  its permissions and its webhook
- [Configuration](https://kritika.home-operations.com/configuration/): the configuration file, sign-in and
  role mappings, GitHub Apps, repository entries, accounts and which
  repositories run
- [Chart values](charts/kritika/README.md)
- [`.kritika.yaml` reference](https://kritika.home-operations.com/repository-config/)
- [Dashboard](https://kritika.home-operations.com/dashboard/): the setup checklist, the Configuration
  page, repository on/off and actions
- [Metrics](https://kritika.home-operations.com/metrics/)
- [Development](https://kritika.home-operations.com/development/): building, testing, evaluation and the
  cluster loop

## License

[AGPL-3.0](LICENSE)
