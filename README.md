<div align="center">

# kritik

**Repository-aware AI pull request review for GitHub.**

[![CI](https://img.shields.io/github/actions/workflow/status/home-operations/kritik/ci.yaml?branch=main&label=ci)](https://github.com/home-operations/kritik/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/actions/workflow/status/home-operations/kritik/release.yaml?branch=main&label=release)](https://github.com/home-operations/kritik/actions/workflows/release.yaml)
[![License](https://img.shields.io/github/license/home-operations/kritik)](https://github.com/home-operations/kritik/blob/main/LICENSE)

</div>

> [!WARNING]
> kritik is not production ready. It is under active development and has no
> release yet: configuration, the database schema and the APIs change without
> notice, and there is no upgrade path from one commit to the next.

kritik indexes a repository, reviews each pull request against that context,
posts one sticky summary comment plus inline findings and a commit status, and
answers follow-ups when the bot is @-mentioned. A pull request from a fork is
reviewed when a maintainer asks with `@<bot> review`. One deployment serves any
number of forge accounts, and every index and review job runs in its own
Kubernetes Job pod that holds no secrets.

## Features

- **Context beyond the diff.** Whole declarations the diff touches,
  definitions of identifiers on changed lines and callers of changed
  declarations, cut by tree-sitter, plus the most similar chunks from a
  VectorChord index of the default branch.
- **An agent, not one prompt.** Each review is a bounded, read-only tool loop
  over the head commit, optionally with allowlisted commands (`gh`, `curl`,
  `fd`, `rg`) so it can read a dependency bump's release notes;
  `agent.maxSteps: 1` makes it one call.
- **Fixes you can apply.** A finding offers its fix as a one-click suggestion,
  with a prompt a coding agent can apply it from.
- **Incremental reviews.** A later push is reviewed against what changed since
  the last review, and `settle` folds a burst of force-pushes into one.
- **Follow-ups.** Someone with write access can @-mention the bot and get an
  answer in the thread.
- **Approvals, opt-in.** A repository or the instance can have a review that
  finds nothing blocking or important approve the pull request, and a later
  review that does withdraw it.
- **Providers and limits.** OpenRouter, OpenAI and Anthropic adapters, with
  per-account concurrency, daily review and monthly token caps. The provider
  key never enters a runner pod: the agent reaches its model through
  kritik's gateway.
- **Repository overrides.** A `.kritik.yaml`, read from the merge-base, can
  narrow the admin's settings and bring its own rules, context files and
  comment templates.
- **Configuration in git.** One YAML file holds the whole configuration,
  read at startup, and a change rolls the pods; secrets stay in Secrets,
  which reach kritik as environment variables.
- **Dashboard.** Sign-in, live review state, full model transcripts, the
  running configuration, repository on/off and an audit log.

## Installing

kritik ships as an OCI Helm chart, `oci://ghcr.io/home-operations/charts/kritik`.
The chart's [README](charts/kritik/README.md) lists every value and shows the
CloudNativePG setup for the three database roles. In short: a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) (and the pgvector it
builds on) loaded, with an owner, an application and a runner role; the one
public URL under `web.url`, which the dashboard and GitHub's webhooks share;
a way to sign in under `auth`; and the configuration file under `config`.
It runs as one Deployment of `kritik serve`, two replicas by default, which
creates a runner Job for each review and index run.

The [setup guide](docs/setup.md) takes a fresh instance through its GitHub
App, model key and embedder to its first review; the dashboard's setup
checklist shows what is still missing.

Security notes:

- Install kritik into a namespace of its own: runner Jobs run in the release
  namespace, and kritik's Role can create, patch and delete every Secret
  there, though it can never get or list one.
- Keep the egress gateway on (the chart's default) with a NetworkPolicy:
  runner pods then reach the outside only through kritik's forward proxy,
  which allows destinations by hostname (github.com and the configuration's
  `egress.allowHosts`), and never hold the credentials `egress.credentials`
  lets the gateway add. Every review needs it, since its model calls go
  through it too.
- Run runner Jobs under a sandboxed RuntimeClass such as gVisor
  (`runner.runtimeClassName`) where the cluster has one, since the pod parses
  untrusted content.

## Documentation

- [Setup](docs/setup.md): from install to the first review, the GitHub App,
  its permissions and its webhook
- [Configuration](docs/configuration.md): the configuration file, sign-in and
  role mappings, GitHub Apps, repository entries, accounts and which
  repositories run
- [Chart values](charts/kritik/README.md)
- [`.kritik.yaml` reference](docs/repository-config.md)
- [Dashboard](docs/dashboard.md): the setup checklist, the Configuration
  page, repository on/off and actions
- [Metrics](docs/metrics.md)
- [Development](docs/development.md): building, testing, evaluation and the
  cluster loop
- [Architecture decision records](docs/adr/), starting with
  [ADR-0002](docs/adr/0002-kritik-pr-review-service.md)

## License

[AGPL-3.0](LICENSE)
