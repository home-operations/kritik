# ADR-0021: one shape for the configuration and `.kritik.yaml`

- **Status:** Proposed
- **Date:** 2026-09-30
- **Authors:** onedr0p.
- **Amends:** [ADR-0010](0010-configuration-layers.md) §2.4 and §2.5
  (the scopes become the defaults, `owner/*` and `owner/name`, and the
  `allow` bounds go), [ADR-0011](0011-runner-tool-images.md) (the tool
  catalog moves to the environment), [ADR-0014](0014-github-app-only-self-hosted.md)
  §2.2 (connections become `apps`), [ADR-0018](0018-rules.md) §2.1 (a rule
  may be a file, and `review.instructions` goes) and
  [ADR-0019](0019-configuration-in-git.md) §2.3 (where `enabled` may be
  written).

> Scope: the keys of the configuration file and of `.kritik.yaml`, where
> each is written, and what moves out of the file. What a review does
> with them is unchanged except where a section says otherwise.

## 1. Context

The configuration grew one feature at a time, and it shows:

- The same settings are written at four levels, `defaults`,
  `accounts[]`, `accounts[].repositories[]` and `.kritik.yaml`, and each
  level merges by its own rules: replace, add up, only tighten, or choose
  within an `allow` bound.
- An account is listed under a connection and configured under
  `accounts[]`, and a repository is two levels down under
  `accounts[].repositories[].name`.
- A reviewer is given guidance four ways: `review.instructions`,
  `review.context`, `review.rules` and `review.agentFiles`.
- `ignore` and `skip.onlyPaths` are two lists of paths.
- `allow` is a bounds system that exists so a `.kritik.yaml` can choose
  models, a mode and agent limits.
- `review.thoroughness` and `review.minSeverity` both decide how much
  kritik says, from different ends, and neither name says so.
- `forge: github` is written everywhere though GitHub is the only forge,
  the embedder repeats a provider's endpoint and key, and polling,
  indexing, retention, runner and tool settings sit next to how a
  repository is reviewed.

kritik has no release, so the shape can change without a migration path.

## 2. Decision

### 2.1 One shape

The configuration's `defaults`, each of its repository entries and a
repository's `.kritik.yaml` take the same keys, so a block can move from
one to another unchanged:

| Key                                                     | `defaults`, `owner/*`, `owner/name` | `.kritik.yaml`                             |
| ------------------------------------------------------- | ----------------------------------- | ------------------------------------------ |
| `mode`                                                  | replaces                            | replaces                                   |
| `models.review`, `models.fallback`                      | replaces                            | replaces, with a model the account may use |
| `feedback`                                              | replaces                            | replaces                                   |
| `comments.inline`                                       | replaces                            | replaces                                   |
| `comments.summaryTemplate`, `comments.inlineTemplate`   | replaces                            | replaces                                   |
| `requireSuggestedFix`                                   | replaces                            | may only turn it on                        |
| `filter`                                                | replaces                            | ANDed with the configuration's             |
| `ignore`                                                | adds up                             | adds                                       |
| `rules`                                                 | adds up by id                       | adds; may not replace a configured id      |
| `context`                                               | replaces                            | adds                                       |
| `agentFiles`                                            | replaces                            | replaces                                   |
| `enabled`                                               | `defaults` and `owner/*` only       | `false` turns the repository off           |
| `agent`, `settle`, `forks`, `incremental.maxDeltaFiles` | replaces                            | not taken                                  |

A value is applied in this order: kritik's default, `defaults`, the
repository's `owner/*` entry, its `owner/name` entry, and its
`.kritik.yaml`. A `.kritik.yaml` naming a key it does not take, or any
unknown key, does not parse, as before.

### 2.2 The configuration file

Its top-level keys are:

- `auth`: sign-in, unchanged.
- `apps`: the GitHub Apps, each a `name` (its hook path,
  `/hooks/<name>`), a `clientId`, given inline or as a secret reference,
  a `privateKey`, a `webhookSecret` and the `accounts` it serves. There
  is no `forge` key and no nested `app` block.
- `providers`: unchanged.
- `embedding`: `model`, a `<provider>/<model>` of an `openrouter` or
  `openai` provider of the instance, whose endpoint and key it uses, and
  `dims`, with the batch bounds as before.
- `egress`: unchanged.
- `defaults`: §2.1's keys for every repository.
- `repositories`: a map from `owner/*` or `owner/name` to §2.1's keys.
  An `owner/*` entry is what an account entry's settings were; there are
  no other patterns. An `owner/name` entry may not set `enabled`: the
  dashboard owns a repository's on or off (ADR-0019 §2.3), and the
  configuration only says where one starts.
- `accounts`: a map from an account's name to what is the account's
  alone, its `limits` and its own `providers`. A model in its `owner/*`
  or `owner/name` entries, or in one of its repositories' `.kritik.yaml`,
  may name one of them.

### 2.3 `.kritik.yaml`

The file is §2.1's keys at the top level; the `review` block goes. It
chooses `mode` and `models` freely: any mode, and any model of a
provider the instance or the account declares. A model of an undeclared
provider is dropped with a note, and the account's `limits` bound what
the choice can cost. `allow`, and the file's choice of agent limits,
commands and settle time, go.

### 2.4 `feedback`

`feedback` replaces `review.thoroughness` and `review.minSeverity`:

| `feedback`           | What is reported                                                                             |
| -------------------- | -------------------------------------------------------------------------------------------- |
| `detailed` (default) | everything a maintainer could act on, nits, missing tests and questions included, all inline |
| `standard`           | the same review, with nits in the summary rather than inline                                 |
| `minimal`            | only bugs, risks and breaking changes                                                        |

A blocking finding is always posted inline, and `comments.inline: false`
still posts the summary alone.

### 2.5 Rules and files

A rule is a `rule`, as before, or a `file` of the repository, read from
the merge base, whose content is the check; either may have `paths`. A
file rule is cited by its id like any other, and its content is capped
with the instruction files' 32 KiB. `review.instructions` goes: a named
instruction file is a file rule. `context` and `agentFiles` stay as they
are.

### 2.6 `ignore` also skips

A pull request whose every changed path is ignored, by kritik's default
globs, the configuration or the `.kritik.yaml`, is skipped, and
`skip.onlyPaths` goes.

### 2.7 Out of the file

Settings about running kritik rather than reviewing a repository move to
the environment, which the chart's values set:

| Was                                    | Now                                            |
| -------------------------------------- | ---------------------------------------------- |
| `polling.interval`, `polling.lookback` | `KRITIK_POLL_INTERVAL`, `KRITIK_POLL_LOOKBACK` |
| `indexing.onboardWindow`               | `KRITIK_ONBOARD_WINDOW`                        |
| `retention.disabledIndexGrace`         | `KRITIK_INDEX_GRACE`                           |
| `retention.transcripts`                | `KRITIK_TRANSCRIPT_RETENTION`                  |
| `runner.activeDeadlineSeconds`         | `KRITIK_RUNNER_DEADLINE`                       |
| `runner.resources`                     | `KRITIK_RUNNER_RESOURCES`, JSON                |
| `tools`                                | `KRITIK_RUNNER_TOOLS`, JSON                    |

An account's own runner deadline and resources go with them. The
environment overlays follow the file: `KRITIK_CONNECTIONS_*` becomes
`KRITIK_APPS_*`, `KRITIK_DEFAULTS_REVIEW_THOROUGHNESS` becomes
`KRITIK_DEFAULTS_FEEDBACK`, and `KRITIK_EMBEDDING_*` keeps only `MODEL`
and `DIMS`.

## 3. Consequences

- A block reads the same in the configuration and in a repository, and a
  repository entry is found by the name the dashboard shows.
- A repository chooses its model without an admin listing it first; an
  account's limits are what stops an expensive choice.
- The file is about what is reviewed and how; a restart, not a reload,
  changes what moved to the environment.
- Every deployment's file and `.kritik.yaml` must be rewritten once.

## 4. Rejected alternatives

- **Keep `allow` for models.** An admin who wants to stop a model can
  leave its provider out; a list per scope was the part people found
  hardest to write.
- **Any glob as a repository key.** Overlapping globs need an order the
  file cannot show; `owner/*` and `owner/name` never overlap in a way
  that needs one.
- **Keep an account's review settings under `accounts`.** They would be
  a second place for what `owner/*` says.
- **Keep two knobs for how much kritik says.** The combinations worth
  having are three, and one ordered key names them.
