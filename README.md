<div align="center">

# kritik

**Repository-aware AI pull request review for GitHub and Forgejo.**

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
answers follow-ups when the bot is @-mentioned. One deployment serves any
number of forge accounts, and every index and review job runs in its own
Kubernetes Job pod that holds no secrets.

## Features

- **Context beyond the diff.** Whole declarations the diff touches,
  definitions of identifiers on changed lines and callers of changed
  declarations, cut by tree-sitter, plus the most similar chunks from a
  VectorChord index of the default branch.
- **Single pass or agentic.** A repository can ask for a bounded, read-only
  tool loop instead of one model call, optionally with allowlisted commands
  (`curl`, `fd`, `rg`) so it can read a dependency bump's release notes.
- **Fixes you can apply.** A finding offers its fix as a one-click suggestion,
  with a prompt a coding agent can apply it from.
- **Incremental reviews.** A later push is reviewed against what changed since
  the last review, and `settle` folds a burst of force-pushes into one.
- **Follow-ups.** Someone with write access can @-mention the bot and get an
  answer in the thread.
- **Providers and limits.** OpenRouter, OpenAI and Anthropic adapters, with
  per-tenant concurrency, daily review and monthly token caps. The provider
  key never enters a runner pod: the agent reaches its model through the
  worker's gateway.
- **Repository overrides.** A `.kritik.yaml`, read from the merge-base, can
  narrow the operator's settings and bring its own instructions and comment
  templates.
- **Dashboard.** Sign-in, dashboard-managed tenants, live review state, full
  model transcripts and an audit log.

GitLab is planned: its webhooks parse, but there is no GitLab client yet.

## Installing

kritik ships as an OCI Helm chart, `oci://ghcr.io/home-operations/charts/kritik`.
The chart's [README](charts/kritik/README.md) lists every value and shows the
CloudNativePG setup for the three database roles. In short: a Postgres with
[VectorChord](https://github.com/tensorchord/VectorChord) (and the pgvector it
builds on) loaded, with an owner, an application and a runner role, the
configuration file under `config.file`, the secrets it references under
`secretMounts`, and optionally an embedder under `embedding` for the index.
`roles.all` runs the single-process topology; `roles.ingest` and
`roles.worker` split it.

Security notes:

- Install kritik into a namespace of its own: runner Jobs run in the release
  namespace, and the worker's Role can create, patch and delete every Secret
  there, though it can never get or list one.
- Keep the egress gateway on (the chart's default) with a NetworkPolicy:
  runner pods then reach the outside only through the worker's forward proxy,
  which allows destinations by hostname (the forges and the file's
  `egress.allowHosts`), and never hold the credentials `egress.credentials`
  lets the gateway add. Agentic reviews need it, since their model calls go
  through it too.
- Run runner Jobs under a sandboxed RuntimeClass such as gVisor
  (`runner.runtimeClassName`) where the cluster has one, since the pod parses
  untrusted content.
- Give a Forgejo installation a read-only `gitToken` beside its `token`:
  runners fetch with `gitToken` when it is set, and otherwise with `token`,
  which can write to the forge.

## Documentation

- [Chart values](charts/kritik/README.md)
- [`.kritik.yaml` reference](docs/repository-config.md)
- [Dashboard](docs/dashboard.md): sign-in, roles, the sealing key and
  transcript retention
- [Metrics](docs/metrics.md)
- [Development](docs/development.md): building, testing, evaluation and the
  cluster loop
- [Architecture decision records](docs/adr/), starting with
  [ADR-0002](docs/adr/0002-kritik-pr-review-service.md)

## License

[AGPL-3.0](LICENSE)
