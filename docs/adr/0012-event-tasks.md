# ADR-0012: event-driven tasks

- **Status:** Proposed
- **Date:** 2026-09-26
- **Amends:** [ADR-0003](0003-forgejo-agentic-review.md) §2.1 (an
  agentic task's runner requires the read-only `gitToken` on Forgejo and
  Gitea, and gets a down-scoped installation token on GitHub) and §2.3 (`.kritik.yaml`
  gains `tasks`), and [ADR-0010](0010-configuration-layers.md) §2.5 (`allow`
  gains `tasks`) and §2.3 (the operator's scopes gain `tasks`).
- **Authors:** perfectra1n.

> Scope: running a declared prompt when a forge event fires, and applying
> what the model's answer asks for once kritik has checked it. Issue triage
> is the first use. It does not change how a pull request review, a
> follow-up or an index run works; the review is not rebuilt on tasks.

## 1. Context

kritik reviews pull requests and nothing else. Every issue event, and every
delivery it has no use for, is dropped at the webhook parser. Issue triage
was the first request beyond reviews, and it generalizes: "when this event
fires, run this prompt with these things, and report back when done".
Hard-coding triage would answer one request and leave the next one
(labelling pull requests by area, summarizing a release, welcoming a first
contributor) to another feature each time.

The pieces a general answer needs mostly exist:

- The webhook parser verifies and decodes every forge's deliveries.
- `repoconfig` reads a repository's `.kritik.yaml`, and ADR-0010 §2.5
  already bounds what it may choose by the operator's `allow`.
- ADR-0005 settled on Go `text/template` for repository-written templates,
  and `internal/prfilter` on CEL for repository-written conditions.
- The agent loop, the gateway (ADR-0004) and the runner pod (ADR-0003 §2.9)
  run a model with read-only tools and no secrets.
- The forge clients post and update sticky comments, and read files at a
  commit.

What does not exist is a way to let a repository say what should happen,
without letting a model (or whoever wrote the issue it reads) write to the
forge beyond what the repository and the operator allowed.

## 2. Decision

A **task** is a small declarative program: a **trigger** (any forge event,
plus a CEL guard), the **context** it gathers, the **prompt** (Go
templates), the **answer** (the custom fields it declares and the actions it
may propose), the **actions** (proposed by the model or driven by rules,
each with a CEL `if`) and the **report** (a templated comment and the
dashboard). Repositories declare tasks in `.kritik.yaml`; the operator turns
tasks on, bounds them and may declare its own. A new `internal/tasks`
package owns the definition, its validation, clipping to the bounds,
matching, rendering, the answer schema and the plan of writes, and does no
I/O itself.

### 2.1 Events: normalized and raw

The webhook parser gains an issue kind, stops dropping issue comments, and
names the subject of every issue, pull request and comment event (its kind,
`issue` or `pull`, and number). Every verified delivery also carries its
forge, the event header verbatim, the action, the sender and the raw JSON
payload, including kinds the review pipeline ignores (`release`, `push`,
`workflow_run` and so on). "Ignored" keeps meaning the review ignores it; a
task may still match it.

Tasks trigger on a normalized vocabulary that reads the same on every
forge (`issue`, `pull_request` and `comment`, each with the forge's action
strings as globs), or on a forge-specific raw event: `raw: { event, actions
}`, globs over the event header and the action.

Forgejo and Gitea spell some actions differently from GitHub. The parser
maps an issue or pull request event's `synchronized` to `synchronize`,
`label_cleared` to `unlabeled` and `label_updated` to `labeled`. Their
`label_updated` covers a label added and a label removed alike, with no
delta in the payload, so on those forges `labeled` means "the labels
changed". This lets one `issue: [labeled]` trigger work on every forge; the
cost is that a label removal on Forgejo or Gitea fires `labeled` tasks too.
The bot-sender guard (§2.5) keeps a task's own label writes from triggering
it again either way.

The raw payload is stored once per delivery some task could match, in a
`task_events` row under row-level security with the existing retention
(the dispatch job, §2.4, deletes it again when no task does), and
jobs reference it by id so the queue's arguments stay small. Tasks see it
decoded, as `raw` in guards and `.Raw` in templates, and the prompt carries
it (cut to 32 KiB) whatever the templates say.

### 2.2 The definition

The implemented surface, with issue triage as the example:

```yaml
tasks:
  - name: triage # [a-z0-9-]; names the task's marker, runs and notes
    on:
      - issue: [opened, reopened]
      - comment: [created]
      - raw: { event: issues, actions: [transferred] }
    if: '!("triaged" in subject.labels)' # CEL over event, subject, raw
    mode: agentic # agentic (default) | single
    models: { review: openrouter/openai/gpt-6-mini }
    agent:
      maxSteps: 20
      timeout: 5m
      tools: [read_file, grep]
      commands: [rg]
    context:
      thread: { comments: 20 }
      files: [{ path: CONTRIBUTING.md }, { glob: "docs/area/*.md", max: 5 }]
      search: [{ name: code, query: "{{ .Subject.Title }}", k: 8 }]
      related: [{ name: dupes, query: "{{ .Subject.Title }}", k: 5 }]
      commands: [{ name: owners, run: cat .github/CODEOWNERS }] # agentic only
    system: .kritik/tasks/system.md.tmpl # after kritik's fixed preamble
    prompt: .kritik/tasks/triage.md.tmpl # or promptInline
    fields:
      priority: { type: string, enum: [p0, p1, p2, p3] }
      needsInfo: { type: boolean }
      missing:
        type: array
        items: { type: string, maxLength: 200 }
        maxItems: 5
    actions:
      comment:
        mode: sticky
        template: .kritik/tasks/triage-comment.md.tmpl
        if: answer.fields.needsInfo
      labels:
        propose: { add: [bug, enhancement], remove: [needs-triage] }
        rules:
          - add: ["priority/{{ .Fields.priority }}"]
          - { add: [needs-info], if: answer.fields.needsInfo }
      state:
        propose: { close: true }
        rules: [{ close: true, if: '"spam" in answer.labels.add' }]
      assign: { propose: { users: [alice, bob] } }
      reviewers: { rules: [{ users: [bob], if: 'subject.kind == "pull"' }] }
      inlineComments: { propose: true }
    enabled: true # false switches off a task of this name
```

`docs/repository-config.md` is the reference for every key, and
`docs/kritik.schema.json` its schema, kept in step with the Go types by
`schema_test.go`. The rules the definition follows:

- **Validated whole, when the file is parsed.** Names, trigger globs, CEL
  guards (a task's `if` sees `event`, `subject` and `raw`; an action's also
  sees `answer`), field declarations, context sources, paths and template
  syntax are checked by `repoconfig.Parse`. Preparing a task parses its
  template files and smoke-renders every template against a sample event
  and a sample answer, as `repoconfig` already does for filters, so a
  template that fails on the shape of the data fails before any event. A
  bad `tasks` block fails the whole `.kritik.yaml`, as any other bad key
  does; the dashboard shows the parse error.
- **Fields are a restricted JSON Schema.** A string with an `enum`,
  `maxLength` or `pattern`; a number or integer with a `minimum` and
  `maximum`; a boolean; an array of those with `maxItems`; or an object of
  those, one level deep. Unset lengths and sizes take defaults, so every
  string and list in an answer is bounded. The fields are compiled into the
  task's answer schema next to a required `summary` and the proposals its
  actions allow, and validated again when the answer is parsed.
- **Templates are sandboxed Go templates** (ADR-0005), with a small
  funcmap of their own rather than sprout's: no `define` or `template`,
  a source cap, and renders bounded in output, loop iterations and bytes
  handed to functions. Prompt templates see the event, the subject, the
  thread (`.Thread`), the context sources and the task. The context
  sources are `.Context.files`, a list of `{path, content}` of the files
  named by path, and each `search` and `related` source under its `name`;
  a `related` source is a list of `{number, title, state, url, isPull,
labels}`. Glob files and `commands` output need a checkout, so only an
  agentic task's runner gathers them, after the templates have rendered:
  it appends each to the prompt in its own `<untrusted>` block, and a
  template never sees them in `.Context`. The names `files` and `notes`
  are reserved. The
  comment and rule templates see the event, the answer and its fields,
  and the comment also what was applied and dropped.
- **Subject-less events write nothing.** A task a raw event without an
  issue or pull request can trigger may declare no action at all; its
  report is its run on the dashboard. Inline comments need a trigger that
  can be about a pull request. A `createIssue` action is deferred (§5).

### 2.3 Actions are declared, and the worker applies them

The model never holds a forge write credential and never calls a tool that
writes. It returns one structured answer; the worker parses it against the
answer schema, turns it into a plan, and applies the plan through its own
forge client.

Actions have two sources and one validator. `propose` lets the model choose
within an allowlist: label globs (offered to the model as an `enum` of the
repository's matching labels), close and/or reopen, a list of users.
`rules` are deterministic, with templated values and a CEL `if` over the
event and the answer. Both feed one plan, which is checked as a whole:

- a label must exist in the repository, and a proposed one must match a
  `propose` glob; when a label is both added and removed, the add wins;
- a user must be a well-formed login with at least read access, and a
  proposed one must be listed; reviews are requested only on a pull
  request, never from its author;
- a state change must be declared, and a rule's wins over the model's; a
  change to the state the subject is already in is dropped;
- an inline comment must be on a pull request and anchored in its diff;
- a block whose `if` is false, or fails, contributes nothing.

Everything left out is recorded with its reason, and the report comment is
rendered last, with what was applied and dropped. Comments carry a hidden
`<!-- kritik:task:<name> -->` marker so a sticky report updates in place;
anything in a rendered comment that could pass for one of kritik's markers
is escaped.

v1's actions are a sticky or appended comment, inline comments, adding and
removing labels, closing and reopening, and assigning users and requesting
reviewers.

### 2.4 Execution

Execution is chosen per task:

- `mode: single` is one structured model call in the worker.
- `mode: agentic`, the default, runs a runner pod with a checkout of the
  default branch and the task's subset of the read-only agent tools, plus
  its allowlisted commands (which is where `context.commands` and glob
  files are gathered, argv split on whitespace, never through a shell).
  The agent gets the `run` tool only when the task lists `run` among its
  tools and names commands for it. The model is reached
  through the gateway (ADR-0004), and the final answer comes back through
  the agent run, as a review's does.

Ingest makes no forge call. It consults only the operator's settings:
when tasks are on and an operator task, or an event the operator lets
repository tasks trigger on, could match the delivery, it stores the
delivery and enqueues one `task_dispatch` job. That job drops the event if
its sender is the installation's bot, resolves the repository's tasks
(`repoconfig.Merged`, §2.6) from the `.kritik.yaml` at the default branch's
tip, records that commit and file on the repository for the dashboard
(§2.8), and enqueues one job per matching task on a `task` queue, unique by
task and event, carrying the commit. The task worker re-resolves the task
at that commit, applies `allow.tasks.maxRunsPerSubjectPerHour` (runs of the
task on the subject that started in the last hour; past it the run is
skipped), admits the run through the model leases and the tenant's token
budget, gathers the context sources (each bounded, and bounded in total),
renders the prompts, runs the model, plans and applies the actions, and
posts the report. An agentic run takes a slot on its model before it does
anything billable; while every slot is held the job is snoozed, which
spends no attempt, with a growing back-off. A `task_runs` row records the
trigger, status, mode, model, fields, and the proposed, applied and dropped
actions; usage flows into `usage` and `model_calls` like a review's.

A failure before the model answers is retried. The answer's charge marks
the run answered (`task_runs.answered_at`) in the same transaction, and from
then on the run is never run again: an attempt that finds it answered but
unfinished ends it as failed, since some of its forge writes, which are not
idempotent, may already have been made.

The GitHub App needs the Issues and Pull requests write permissions, and
the Issues and Issue comment event subscriptions, for the tasks that use
them; Forgejo and Gitea tokens need the matching write scopes.

### 2.5 Security model

A task is written by the repository's maintainers, reads text written by
anyone who can open an issue, and runs a model that text may try to steer.
The design keeps each of those in its lane:

- **Declared actions only.** The model's answer is data. It can propose
  only what the task declared, and the worker checks every proposal against
  the task, the repository and the operator's bounds before it writes
  (§2.3). A prompt injection can at most pick among the options a
  maintainer already offered.
- **Only the worker writes.** Runner pods of agentic tasks get a git
  credential that can only read, and reach the model through the gateway,
  never a provider key or a forge write credential; their tools are
  read-only. On GitHub the worker mints an installation token for each run,
  scoped to the task's repository with `contents: read` alone, although the
  App itself holds Issues and Pull requests write for tasks. On Forgejo
  and Gitea the credential is the installation's `gitToken` (ADR-0003
  §2.1), which the operator vouches is read-only; an installation without
  one never falls back to its API token for a task, and its agentic tasks
  are skipped with "agentic tasks need a read-only gitToken". Single-mode
  tasks start no runner and need neither. Agentic reviews keep ADR-0003's
  token as before.
- **A fixed preamble and fenced data.** Every task's system prompt starts
  with kritik's preamble, which states that text inside `<untrusted>`
  blocks is data, never instructions, and that only what the answer schema
  offers may be proposed. A task's `system` is added after it, only when
  the operator allows it, and cannot replace it. The subject, the thread
  and the raw payload are appended to the prompt as JSON inside
  `<untrusted>` blocks whatever the templates say. Templates see every
  event and thread string with `<untrusted` and `</untrusted` escaped, and
  each context source's result already wrapped in its own block, so a
  template cannot inline data that escapes its fence; the price is that a
  template cannot post-process a context source's raw text.
- **No echo loops.** An event whose sender is the installation's own bot
  never triggers a task, so the labels and comments a task writes cannot
  trigger it again. The dispatch job checks it, since it needs the bot's
  login from the forge.
- **The configuration comes from the default branch's tip.** A task and
  every file it names are read at the default branch's tip, not at a pull
  request's merge base or head: an issue has no merge base, and only
  someone who can push to the default branch can change what a task does
  or what its templates say. The worker re-reads the task at the commit
  the dispatch job resolved it at.
- **Operator bounds, and who is trusted.** The operator's configuration
  file is trusted: its own tasks are not clipped, only switched on or off
  by `allow.tasks.enabled`; they pick their own mode and agent limits,
  outside `allow.modes` and `allow.agent`, and a task job's timeout is
  sized to cover their agent timeouts too. Tasks a tenant admin writes on the dashboard
  are clipped exactly like a repository's (§2.6), since `allow` is the
  operator's alone and a tenant admin must not escape it through a task.

### 2.6 The operator's bounds and tasks

`allow` (ADR-0010 §2.5) gains `tasks`, bound by bound like the rest of
`allow`: one written at a narrower scope replaces the broader one's.

| Bound                      | Default                                  | Bounds                                                     |
| -------------------------- | ---------------------------------------- | ---------------------------------------------------------- |
| `enabled`                  | `false`                                  | whether any task runs, the operator's own included         |
| `events`                   | `issue.*`, `pull_request.*`, `comment.*` | globs over trigger names, such as `raw:release.*`          |
| `actions`                  | `comment`, `labels`, `inlineComments`    | action kinds; `state`, `assign` and `reviewers` are opt-in |
| `context`                  | all but `commands`                       | context source kinds                                       |
| `tools`                    | `read_file`, `grep`, `list_files`        | agent tools                                                |
| `systemPrompt`             | `false`                                  | whether a task may add to the system prompt                |
| `repositoryTasks`          | `true`                                   | whether `.kritik.yaml` may define tasks                    |
| `maxTasks`                 | 10                                       | a repository's tasks                                       |
| `maxRunsPerSubjectPerHour` | 6                                        | runs of one task on one issue or pull request              |
| `maxFields`                | 16                                       | a task's fields                                            |

A task's trigger names are `issue.opened`, `raw:release.published` and so
on, with `*` for a trigger that lists no actions. A task's `mode` must be
one of the existing `allow.modes` (an agentic task costs a runner pod), its
models come from `allow.models`, its agent limits are held to
`allow.agent`, and its commands, agent and context alike, to
`allow.commands`, reusing ADR-0010's choose, at-most and subset bounds.
`allow.tasks.tools` is the one new knob for the agent's tools.

`defaults`, tenants and repository entries gain `tasks`, the operator's
own. The policy row is `tasks` at every scope, writable by a tenant admin,
and appended to by the repository. Operator tasks merge by name across the
scopes, a narrower scope's replacing a broader one's where it stands, so a
tenant cannot silently hide a default's task; a task with `enabled: false`
switches off the one it names, which is also the only way to remove it.

`repoconfig.Merge` resolves the tasks that run: the configuration file's
operator tasks, unclipped; then the dashboard's, clipped; then the
repository's, clipped, less any sharing an operator task's name. Clipping
drops what is out of bounds rather than the whole task where it can (an
event, an action block, a context source, a tool, the `system` prompt,
fields past `maxFields`, a model or limit), and drops a task left with no
trigger or with a mode outside `allow.modes`. Each drop is a note with the
task, what was dropped and why (`Merged.TaskNotes`), and the dashboard
shows them beside each repository's resolved tasks and where each came
from. Callers take tasks only from `Merged.Tasks`; the unclipped lists in
`configfile.Settings` are its input.

### 2.7 Forge clients

`forge.Client` gains reading an issue, the repository's labels, adding and
removing labels, closing and reopening, assigning users, requesting
reviewers and searching issues (for the `related` context source), on
GitHub and on Forgejo and Gitea. Comments, conversations, reviews,
permissions, file reads and branch tips reuse what exists.

### 2.8 Dashboard

A repository's page lists its resolved tasks: their names, where each came
from (the configuration file, the dashboard or `.kritik.yaml`), triggers,
mode and action kinds, with the clipping notes. They are resolved from the
`.kritik.yaml` the last dispatch read at the default branch's tip, which
it records on the repository (migration 0014); before any dispatch, from
the one the last review read at its merge base, and the page says which. Task runs, their fields,
the three action lists and a transcript link join it once runs are
recorded, with live updates.

## 3. Consequences

- A repository can automate issue and pull request housekeeping with a
  file and two templates, and kritik gains no feature code per use: triage
  ships as a documented recipe, tested like any other.
- Tasks are off until the operator turns them on, and a new action kind,
  raw event or context kind is off until the operator lists it.
- Everything a task can write is visible in its definition, and every
  write is recorded with what the model proposed and what was dropped.
- `internal/tasks` is a large surface in one pure, table-tested package.
- A typo in `tasks` stops the whole `.kritik.yaml` from applying, the
  review's settings included, until it is fixed.
- On Forgejo and Gitea, removing a label fires `labeled` tasks.
- The installation's credentials need write access to issues and pull
  requests, beyond what reviews need; the GitHub App needs the Issues
  permission and event subscriptions.
- Templates cannot post-process a context source's text, which stays
  fenced, and an agentic task's glob files and command output reach the
  prompt but not its templates.
- What a run's context left out, the worker's cuts and the runner's, is
  recorded in the run's notes.
- Upgrading mid-flight: while workers of the previous release still run
  after migration 0013, one of them can refuse an agentic task's gateway
  step (its token has no review), and a previous release's runner refuses
  a task spec; either run fails without writing, and reviews are
  unaffected.

## 4. Rejected alternatives

- **A hard-coded triage feature.** Quicker for one use, but every later
  request (area labels, release notes, welcome messages) would be another
  feature with its own configuration, and the security review would be
  repeated for each. The general form costs one package and one review.
- **An agent with live forge tools.** Giving the model tools that label,
  close or comment directly would let text in an issue steer real writes,
  and would put a write credential in the runner pod, against ADR-0003
  §2.1 and ADR-0004. A declared, validated answer applied by the worker
  keeps the model's reach to the options a maintainer offered.
- **Comment-only tasks.** Letting a task only post a comment is safe, but
  leaves the actual triage (labels, state, assignees) to a human reading
  the comment; the plan-and-validate step gives the same safety for the
  writes that matter.

## 5. Deferred

- A `createIssue` action, so a subject-less event such as a release can
  report somewhere other than the dashboard.
- Rebuilding the pull request review on tasks.
- GitLab, which has no `forge.Client` yet.
- Letting a broken `tasks` block leave the rest of `.kritik.yaml` in force.
- The operator's "tasks bounds" view in the operator console.
