# `.kritik.yaml` reference

A repository may commit an optional `.kritik.yaml` at its root to tune how
kritik reviews it. It is read from the merge-base commit, never the pull
request's own tree, so a pull request cannot use its own copy to weaken the
review applied to it. kritik reads it before the review starts, and applies
it to follow-ups (from the pull request's merge base) and to indexing (from
the commit indexed) too. Its [`tasks`](#tasks) are read from the default
branch's tip instead.

The file holds nothing secret: no field takes a credential, a URL, a host
or a secret reference, and it can only name what the operator configured,
a model by its `<provider>/<model>` reference and a command by its name.

[`kritik.schema.json`](kritik.schema.json) is its JSON Schema. An editor
using the YAML language server validates the file as it is written when
its first line names the schema:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/home-operations/kritik/main/docs/kritik.schema.json
```

## What it may change

The file narrows what the operator allows, adds to the review's
instructions, and chooses a few settings within bounds the operator sets:

- `enabled: false`: stops reviews, follow-ups and indexing for the
  repository. It cannot turn a disabled repository back on.
- `filter`: a filter expression ANDed with the operator's own. It is
  compiled and smoke-tested against a sample pull request when the file is
  parsed, so a broken expression is rejected rather than silently skipping
  every review. A review it filters out ends before any runner starts.
- `ignore`: path globs added to the operator's own ignore list, for
  reviews and indexing alike.
- `skip.onlyPaths`: path globs. A pull request is skipped only when every
  changed path matches at least one of them.
- `review.instructions`: paths to files, read from the same merge-base
  tree, appended after the operator's instructions to the reviewer's system
  prompt (and a follow-up's), capped at 32 KiB joined. An entry may instead
  be a `path` with `paths` globs, included only when a changed path matches
  one of them, so rules for one part of the repository do not spend the cap
  on changes elsewhere:

  ```yaml
  review:
    instructions:
      - .kritik/rules.md
      - { path: .kritik/sql.md, paths: ["internal/store/**", "**/*.sql"] }
  ```

- `review.context`: files that explain the code, each a `path` with a
  `description` and optional `paths` globs, added after the operator's. An
  agentic review is pointed at each file to read it with its own tools; a
  single-shot review is given its content, after the diff and before the
  context kritik gathers, as the prompt budget allows. A file with `paths`
  applies only when a changed path matches one of them:

  ```yaml
  review:
    context:
      - path: internal/store/migrations/0001_init.sql
        description: the schema; check queries against it
        paths: ["internal/store/**"]
  ```

- `review.requireSuggestedFix: true`: findings must include a suggested
  fix. The file can turn the requirement on, never off.
- `review.minSeverity`: `nit` or `important`, the least severe finding
  posted as an inline comment. A `blocking` finding is always posted, and
  the summary still lists every finding.
- `review.inlineComments: false`: posts the summary alone, without inline
  comments.
- `review.templates.summary` / `review.templates.inline`: paths to Go
  [text/template](https://pkg.go.dev/text/template) templates that replace
  kritik's built-in summary and inline comment templates, with the
  [sprout](https://github.com/go-sprout/sprout) helpers tuppr and chaski
  expose (std, strings, conversion, encoding, numeric, slices, maps, regex,
  time, semver and reflect; not env, filesystem, network, random, uniqueid
  or checksum, and not `set` or `unset`). The `template`, `define` and
  `block` actions are refused, so a template cannot read any file or call
  any other template. The summary template's dot is the review (`.Number`,
  `.HeadSHA`, `.Model`, `.Result.Summary.Take`, `.Result.Summary.Praise`,
  `.Result.Findings`, `.Counts.Blocking`/`.Important`/`.Nit`, `.Notes`,
  `.Incremental`, `.PriorHeadSHA`, `.Incomplete`). The inline template's dot
  is one finding (`.Path`, `.Line`, `.EndLine`, `.Severity`, `.Title`,
  `.Explanation`, `.SuggestedFix`, `.Replacement`, `.AgentPrompt`, `.URL`, a
  link to the lines at the head commit). Rendering is bounded (loop
  iterations, bytes per function call, output size, a deadline), so a
  template cannot hang or exhaust memory; one that exceeds a bound falls
  back to the default with a note in the comment.

## What it may choose within the operator's bounds

These choose a value for the repository, each within a bound the operator
sets in an `allow` block (at `defaults`, a tenant or a repository entry).
Where the operator sets no bound, the file may only pick the operator's own
value, or a limit or settle time at or below it:

- `mode`: `single` or `agentic`, from `allow.modes`.
- `models.review` / `models.fallback`: a `<provider>/<model>` from
  `allow.models`, used for the review and for follow-ups.
- `agent.maxSteps`, `agent.maxToolOutputBytes`, `agent.maxTokens`,
  `agent.timeout`: each at most its `allow.agent` bound.
- `agent.commands`: a subset of `allow.commands`.
- `settle`: how long a new head waits before its review starts, at most
  `allow.settle`.

```yaml
mode: agentic
models: { review: openrouter/openai/gpt-6-mini }
agent: { maxSteps: 40, commands: [rg] }
settle: 5m
filter: '!pr.body.contains("[skip-review]")'
ignore: ["web/src/generated/**"]
skip: { onlyPaths: ["docs/**"] }
review:
  instructions: [".kritik/rules.md"]
  requireSuggestedFix: true
```

A value outside its bound is dropped, not clamped: the operator's value
applies for that field, a note in the review's summary says which field
was dropped and what was allowed, and the rest of the file still applies.
`limits`, `forks`, `runner`, `incremental` and `agent.commandTimeout` are
never the repository's to choose; a file naming one of them, or any other
unknown key, does not parse.

## Filter recipes

`filter` is a [CEL](https://cel.dev) expression over `pr`, which has the
pull request's `number`, `title`, `body`, `author`, `state`, `open`,
`merged`, `draft`, `fork`, `headRef`, `headSha`, `baseRef`, `url`,
`createdAt` and `labels` (each with a `name` and a `color`), and `event`,
what started the review: `opened`, `reopened`, `ready_for_review`,
`synchronize` (a push), `poll` (a push kritik found without its webhook)
or `manual` (a re-run from the dashboard).

Some filters, each the whole `filter` value:

- Skip drafts: `!pr.draft`
- Skip anything labelled `skip-review`:
  `!pr.labels.exists(l, l.name == "skip-review")`
- Skip Renovate's pull requests: `!pr.author.startsWith("renovate")`
- Review only pull requests into `main`: `pr.baseRef == "main"`
- Skip when the description asks to: `!pr.body.contains("[skip-review]")`
- Review when a pull request opens or is re-run, not on every push:
  `pr.event != "synchronize" && pr.event != "poll"`

## Tasks

`tasks` declares event-driven tasks ([ADR-0012](adr/0012-event-tasks.md)):
when a forge event fires, run a prompt with the context the task names, have
the model answer with the fields the task declares and the actions it may
propose, then apply what survives validation and report back. Issue triage
is one such task, written in [the recipes](#task-recipes) below, not a
feature of its own.

Tasks run only when the operator turns them on (`allow.tasks.enabled`, see
[the operator's bounds](#the-operators-bounds-on-tasks)), and every
repository task is clipped to those bounds. A task, and every file it names,
is read from the tip of the default branch, not a pull request's merge
base: an issue has no merge base, and only people who can push to the
default branch should be able to change what a task does. A `tasks` block
that does not parse makes the whole file fail to parse, as any other bad key
does.

The model never writes to the forge. It returns a structured answer, and
kritik's worker checks every value in it against what the task declares and
what the repository has before it applies anything. A runner pod that runs
an agentic task holds only the installation's read-only credential.

### A task's keys

| Key                                                                              | What it does                                                                                                                                                                                                                                                                                                                                                                         |
| -------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `name`                                                                           | Required. Lowercase letters, digits and `-`, at most 63, starting with a letter or digit; unique in the file. It names the task in its sticky comment's marker, its runs and its notes. A repository task with the name of an operator task is dropped.                                                                                                                              |
| `enabled`                                                                        | `false` switches the task off, and then only `name` is checked. Unset is on.                                                                                                                                                                                                                                                                                                         |
| `on`                                                                             | Required. The [triggers](#triggers); the task runs when any of them fires.                                                                                                                                                                                                                                                                                                           |
| `if`                                                                             | A [CEL guard](#guards) over `event`, `subject` and `raw`; the task runs only when it is true. Empty always runs.                                                                                                                                                                                                                                                                     |
| `mode`                                                                           | `agentic` (the default) runs the task in a runner pod with a checkout of the default branch, the read-only agent tools and a git credential that can only read (on Forgejo and Gitea the installation's `gitToken`, without which the task is skipped); `single` is one structured model call in the worker. It must be one of the operator's `allow.modes`, or the task is dropped. |
| `models.review`, `models.fallback`                                               | `<provider>/<model>` references from `allow.models`; one outside it is dropped and the operator's model applies.                                                                                                                                                                                                                                                                     |
| `agent.maxSteps`, `agent.maxToolOutputBytes`, `agent.maxTokens`, `agent.timeout` | Each at most its `allow.agent` bound, or the operator's own value; one over it is dropped.                                                                                                                                                                                                                                                                                           |
| `agent.tools`                                                                    | The agent tools an agentic run gets, from `allow.tasks.tools`. Unset is every tool the operator allows; `[]` is none; `run` needs `agent.commands`.                                                                                                                                                                                                                                  |
| `agent.commands`                                                                 | Command names the agent may run, a subset of `allow.commands`, or of the operator's own `agent.commands` when it sets none.                                                                                                                                                                                                                                                          |
| `context`                                                                        | The [context sources](#context-sources) a run gathers.                                                                                                                                                                                                                                                                                                                               |
| `system`                                                                         | A path to a template added after kritik's fixed system preamble, only when the operator sets `allow.tasks.systemPrompt`.                                                                                                                                                                                                                                                             |
| `prompt`                                                                         | A path to the prompt template.                                                                                                                                                                                                                                                                                                                                                       |
| `promptInline`                                                                   | The prompt template itself; set `prompt` or `promptInline`, not both. With neither, a built-in prompt asks for a summary and the fields.                                                                                                                                                                                                                                             |
| `fields`                                                                         | The custom [answer fields](#fields).                                                                                                                                                                                                                                                                                                                                                 |
| `actions`                                                                        | What the answer may lead to: [actions](#actions).                                                                                                                                                                                                                                                                                                                                    |

Paths are relative to the repository root and may not leave it.

### Triggers

Each entry of `on` has exactly one key:

- `issue: [actions]`: an issue, never a pull request. Actions include
  `opened`, `edited`, `reopened`, `closed`, `labeled`, `unlabeled` and
  `assigned`.
- `pull_request: [actions]`: a pull request. Actions include `opened`,
  `synchronize`, `reopened`, `ready_for_review`, `closed`, `labeled` and
  `unlabeled`.
- `comment: [actions]`: a comment on an issue or a pull request, including a
  review comment on a pull request's lines; `created` or `edited`.
- `raw: { event: <name>, actions: [actions] }`: any delivery whose event
  header (`X-GitHub-Event`, `X-Gitea-Event`) matches `event`, such as
  `release`, `push` or `workflow_run`, whether or not kritik otherwise acts
  on it. The names are the forge's own, so a raw trigger is forge-specific.

The actions are the forge's `action` strings, as globs; an empty or missing
list is any action. Forgejo and Gitea spell some of them differently, and
kritik normalizes an issue or pull request event's action to GitHub's
spelling, whether a normalized or a raw trigger matches it: `synchronized`
is `synchronize`, `label_cleared` is `unlabeled`, and `label_updated` is
`labeled`; other events keep the forge's own spelling. A Forgejo or Gitea `label_updated` does not say
whether a label was added or removed, so there `labeled` means "the labels
changed". An event kritik's own bot account sent never triggers a task, so
the labels a task adds cannot trigger it again.

The operator's bounds glob a trigger's event names: `issue.opened`,
`pull_request.synchronize`, `comment.created`, or `raw:<event>.<action>`
such as `raw:release.published`, with `*` for a trigger that lists no
actions.

### Guards

A task's `if` and every action's `if` are [CEL](https://cel.dev)
expressions that must be boolean. A guard sees:

- `event`: `forge` (`github`, `forgejo` or `gitea`), `name` (`issue`,
  `pull_request`, `comment`, or `""` for a raw event), `rawEvent` (the event
  header), `action`, `sender`, and `repo` with `owner`, `name` and
  `defaultBranch`.
- `subject`: the issue or pull request: `kind` (`issue` or `pull`),
  `number`, `title`, `body`, `state`, `author`, `url`, `labels`,
  `assignees` and `draft` (true for a draft pull request, from the forge
  or, when the forge is too old to report it, from the delivery). An event
  with no issue or pull request has every key, with `kind` empty, so
  `subject.kind == "issue"` is false rather than an error.
- `raw`: the decoded payload, as the forge sent it.
- `answer`, in an action's `if` only (and a rule's): the model's validated
  answer, with `summary`, `comment`, `fields`, `labels.add`,
  `labels.remove`, `state`, `assignees`, `reviewers` and `inline` (each
  with `path`, `line`, `end_line` and `body`). Every key is present, empty
  when the task does not offer it. A task's own `if` using `answer` does
  not parse.

Evaluation is cost-limited, since `raw` is up to the webhook body's 4 MiB.

### Templates

Templates are Go [text/template](https://pkg.go.dev/text/template), run in
a sandbox: `define` and `template` are refused, a template's source is
capped at 64 KiB, and a render is bounded in loop iterations, bytes handed
to functions and output size (256 KiB for a prompt, 64 KiB for a comment,
1 KiB for a rule value or query). Missing map keys render as their zero
value. Every template is parsed and smoke-rendered against a sample event
and a sample answer when the task is prepared, so one that fails on the
shape of the data fails there rather than on an event.

The functions are `join`, `lower`, `upper`, `trim`, `truncate` (to a number
of runes), `toJSON`, `default`, `contains` (a string or a list),
`hasPrefix`, `quote`, `fence`, `printf` (widths and precisions at most
1024), `print`, `println`, `html`, `js` and `urlquery`, as well as
text/template's builtins such as `index`, `len`, `eq`, `and`, `or` and
`not`. Note the argument order: `{{ join ", " .Subject.Labels }}`,
`{{ truncate 80 .Subject.Title }}`, `{{ default "none" .Fields.area }}`,
`{{ contains "bug" .Subject.Labels }}`, `{{ hasPrefix "area/" . }}`.

What a template sees depends on what it renders:

| Variable                                                                                                                                                           | `system`, `prompt`, `promptInline` | `context` queries | `actions.comment.template`, rule values |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- | ----------------- | --------------------------------------- |
| `.Forge`, `.Event`, `.RawEvent`, `.Action`, `.Sender`                                                                                                              | yes                                | yes               | yes                                     |
| `.Subject` (`.Kind`, `.Number`, `.Title`, `.Body`, `.State`, `.Author`, `.URL`, `.Labels`, `.Assignees`, `.Draft`); nil without one                                | yes                                | yes               | yes                                     |
| `.Raw`, the decoded payload                                                                                                                                        | yes                                | yes               | yes                                     |
| `.Repo` (`.Owner`, `.Name`, `.DefaultBranch`)                                                                                                                      | yes                                | yes               | yes                                     |
| `.Task`, the task's definition                                                                                                                                     | yes                                | yes               | yes                                     |
| `.Thread`, the subject's comments (`.Author`, `.Body`, `.CreatedAt`)                                                                                               | yes                                |                   |                                         |
| `.Context.files` and `.Context.<name>`, context sources' results                                                                                                   | yes                                |                   |                                         |
| `.Answer` (`.Summary`, `.Comment`, `.Fields`, `.Labels.Add`, `.Labels.Remove`, `.State`, `.Assignees`, `.Reviewers`, `.Inline`) and `.Fields`, the answer's fields |                                    |                   | yes                                     |
| `.Applied` (`.AddLabels`, `.RemoveLabels`, `.State`, `.Assignees`, `.Reviewers`, `.Inline`) and `.Dropped` (each `.Action`, `.Value`, `.Reason`)                   |                                    |                   | comment only                            |

Everything in an event comes from people who may try to steer the model, so
the prompt keeps it apart from instructions whatever the templates say:

- The system prompt always starts with kritik's fixed preamble: data inside
  `<untrusted>` blocks is never instructions, and only what the answer
  schema offers may be proposed. A task's `system` is added after it and
  cannot replace it.
- The subject, the thread and the raw payload (cut to 32 KiB) are appended
  to the prompt as JSON inside `<untrusted>` blocks.
- In the prompt templates every string of the event and thread has
  `<untrusted` and `</untrusted` escaped, so text a template inlines cannot
  open or close a block, and every `.Context` value is already inside its
  own `<untrusted>` block. `{{ fence "name" .X }}` wraps any other value
  the same way.
- A report comment has anything that could pass for one of kritik's hidden
  markers escaped.

### Context sources

`context` names what a run gathers besides the event, each a kind the
operator allows in `allow.tasks.context`:

- `thread: { comments: N }`: the subject's last `N` comments (at most 200),
  as `.Thread`, not in `.Context`.
- `files`: files from the default branch, each a `path`, or a `glob` with
  a `max` of at most 50 files, as `.Context.files`: a list of
  `{path, content}`.
- `search`: `[{ name, query, k }]`, the `k` (at most 50) chunks of the
  repository's index most like `query`, as `.Context.<name>`.
- `related`: `[{ name, query, k }]`, the forge's issues and pull requests
  matching `query`, such as likely duplicates, as `.Context.<name>`: a list
  of `{number, title, state, url, isPull, labels}`. The query is free text:
  on GitHub every word with a colon (a qualifier such as `repo:` or
  `is:open`) is dropped, since the query usually holds an issue author's
  title, and issues and pull requests are searched apart and interleaved.
  A `search` or `related` source that fails is left out with a note; the
  run goes on without it.
- `commands`: `[{ name, run }]`, agentic tasks only: a command line run in
  the runner pod without a shell, split on whitespace, whose first word must
  be in `allow.commands` (or the operator's own `agent.commands` when it
  sets none), appended to the prompt in its own block under its `name`
  (not in a template's `.Context`, see below). Shell characters (`| ; & $ < > ( ) { } * ? ~`,
  quotes and backslashes) are refused. Off unless the operator lists
  `commands` in `allow.tasks.context`.

A `search`, `related` or `commands` source is keyed by its `name`, which
must start with a lowercase letter and hold only letters, digits and `_`,
unique across them; `files` and `notes` are reserved. A `query` is a template over the event. Every
`.Context` value reaches a template already inside its own `<untrusted>`
block, as JSON when it is not text, so `{{ .Context.files }}` inlines the
whole list and a template cannot pick out its fields.
`{{ range .Context }}{{ . }}{{ end }}`, as the built-in prompt does, puts
every gathered source in the prompt.

Glob files and command output need a checkout, so only an agentic task
gets them: its runner gathers them after the templates have rendered and
appends each to the prompt in its own `<untrusted>` block, so they are not
in a template's `.Context`, and `.Context.files` holds the `path` files
alone. A single-mode task leaves a glob out. Each source is cut to 32 KiB
and all of them to 128 KiB; what a run left out, in the worker or the
runner, is listed in its notes. An agentic task's agent gets the `run`
tool only when `agent.tools` lists `run` (which `allow.tasks.tools` must
allow for a repository's task) and `agent.commands` names commands for it.

### Fields

`fields` declares the custom values the model answers with, beside the
required `summary`, as a restricted JSON Schema, in the order written:

- `string`, with `enum`, `maxLength` (1 to 10000; 1000 when unset) or
  `pattern` (a Go regular expression);
- `number` or `integer`, with `minimum` and `maximum`;
- `boolean`;
- `array` of one of those, with `items` and `maxItems` (1 to 100; 20 when
  unset);
- `object` of those, one level deep, with `properties`.

Any may have a `description`. A field name starts with a lowercase letter
and holds only letters, digits and `_`. Every field is required in the
answer, and an answer that breaks the schema is rejected as a whole. The
fields are `.Fields` in output templates and `answer.fields` in action
guards, and the dashboard shows them with the run.

### Actions

`actions` declares what the answer may lead to; a block left out is off.
Most blocks take a model's proposal (`propose`), rules that apply whatever
the model says (`rules`, each with templated values and an `if`), or both,
and an `if` over the whole block. Both feed one plan, which is checked as a
whole before anything is written, and everything left out is recorded with
its reason.

- `comment: { mode, template, if }`: the report. `sticky` (the default)
  updates one comment per task and subject, `append` posts a new one each
  run, `none` posts nothing. `template` is a path to the comment template;
  without one the report is the summary, the answer's `comment` and what
  was applied. When the comment is on, the answer gains a `comment` string.
- `labels: { propose: { add, remove }, rules: [{ add, remove, if }], if }`:
  `propose` lists label globs the model may add or remove; the answer schema
  offers only the repository's labels that match. Each rule's `add` and
  `remove` are templates, rendered against the answer. Every label must
  exist in the repository; when a label is both added and removed, the add
  wins.
- `state: { propose: { close, reopen }, rules: [{ close | reopen, if }], if }`:
  close or reopen the subject. A rule sets exactly one of them, and a rule
  wins over the model.
- `assign: { propose: { users }, rules: [{ users, if }], if }`: assign
  users. The model picks among `propose.users`; rule values are templates.
  A user must be a valid login with at least read access to the repository.
- `reviewers`: the same shape as `assign`, to request reviews; pull
  requests only, and never from the author.
- `inlineComments: { propose: true, if }`: comments on lines the pull
  request changes; lines outside the diff are dropped. It needs a trigger
  that can be about a pull request.

Globs are [doublestar](https://github.com/bmatcuk/doublestar) globs, for
labels as for paths: `*` matches within one `/`-separated segment, so
`area/*` matches `area/web` but not `area/web/ui`, and `*` alone matches
only labels without a `/`; `**` matches across segments. The same holds for
trigger actions and the operator's event globs.

A task that a subject-less event can trigger (a `raw` trigger on anything
but `issues`, `issue_comment`, `pull_request`, `pull_request_review_comment`
or `pull_request_comment`) may declare no action at all: there is no issue
or pull request to write to, so its report is the run on the dashboard.

### The operator's bounds on tasks

The operator writes `allow.tasks` at `defaults`, a tenant or a repository
entry, bound by bound like the rest of `allow`:

| Bound                      | Default                                      | What it bounds                                                                       |
| -------------------------- | -------------------------------------------- | ------------------------------------------------------------------------------------ |
| `enabled`                  | `false`                                      | Whether any task runs, the operator's own included.                                  |
| `events`                   | `["issue.*", "pull_request.*", "comment.*"]` | Globs over trigger event names; a raw event must be listed, such as `raw:release.*`. |
| `actions`                  | `[comment, labels, inlineComments]`          | Action kinds; `state`, `assign` and `reviewers` must be listed.                      |
| `context`                  | `[thread, files, search, related]`           | Context source kinds; `commands` must be listed.                                     |
| `tools`                    | `[read_file, grep, list_files]`              | Agent tools; `run` is opt-in.                                                        |
| `systemPrompt`             | `false`                                      | Whether a task may add to the system prompt.                                         |
| `repositoryTasks`          | `true`                                       | Whether a `.kritik.yaml` may define tasks at all.                                    |
| `maxTasks`                 | `10`                                         | A repository's tasks.                                                                |
| `maxRunsPerSubjectPerHour` | `6`                                          | How often one task runs on one issue or pull request; a run past it is skipped.      |
| `maxFields`                | `16`                                         | A task's fields.                                                                     |

A task's mode, models, agent limits and commands are bounded by
`allow.modes`, `allow.models`, `allow.agent` and `allow.commands`, as for
the review.

The operator may also define tasks, under `tasks` at `defaults`, a tenant
or a repository entry. They merge by name: a narrower scope's task replaces
a broader scope's of the same name, and one with `enabled: false` switches
it off. Tasks the operator's configuration file defines are trusted: only
`allow.tasks.enabled` applies to them. Tasks a tenant admin writes on the
dashboard, and a repository's own, are clipped to every bound above
(`repositoryTasks` aside, for the dashboard's), and to the mode, model,
agent and command bounds. Clipping
drops what is out of bounds, not the whole task where it can: an event, an
action block, a context source, a tool, the `system` prompt or fields past
`maxFields`. A task left with no trigger, or with a mode the operator does
not allow, is dropped. The dashboard lists each repository's tasks, where
each comes from and a note for everything clipped, from the `.kritik.yaml`
the last task event read at the default branch tip (before any event, the
one the last review read, and the page says which).

```yaml
# The operator's configuration file.
defaults:
  allow:
    modes: [single, agentic]
    tasks:
      enabled: true
      events: ["issue.*", "pull_request.*", "comment.*", "raw:release.*"]
      actions: [comment, labels, inlineComments, assign]
```

### Task recipes

Each recipe is a directory under [`recipes/`](recipes/) laid out as it would
be in a repository, and kritik's tests parse and prepare every one of them.

#### Issue triage

Label a new issue by type, priority and area, ask its author for what is
missing, and point out duplicates. The model picks the type label from a
short list; the rules add the priority, area and `triaged` labels from its
fields, so the `if` leaves an issue alone once it is triaged.

[`recipes/issue-triage/.kritik.yaml`](recipes/issue-triage/.kritik.yaml):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/home-operations/kritik/main/docs/kritik.schema.json
tasks:
  - name: triage
    on:
      - issue: [opened, reopened]
    # Once triaged, an issue is left alone: the rules below add the label.
    if: '!("triaged" in subject.labels)'
    mode: single
    context:
      thread: { comments: 20 }
      files:
        - path: CONTRIBUTING.md
      related:
        - { name: dupes, query: "{{ .Subject.Title }}", k: 5 }
    prompt: .kritik/tasks/triage.md.tmpl
    fields:
      priority: { type: string, enum: [p0, p1, p2, p3] }
      area: { type: string, enum: [api, web, runner, docs] }
      needsInfo:
        type: boolean
        description: The report lacks what a maintainer needs to act on it.
      missing:
        type: array
        items: { type: string, maxLength: 200 }
        maxItems: 5
    actions:
      comment:
        mode: sticky
        template: .kritik/tasks/triage-comment.md.tmpl
        if: 'answer.fields.needsInfo || answer.comment != ""'
      labels:
        # The model picks the type label; kritik offers only these, and
        # only those the repository has.
        propose:
          add: [bug, enhancement, question, documentation]
          remove: [needs-triage]
        # The rules add labels from the fields, whatever the model proposed.
        rules:
          - add:
              - "priority/{{ .Fields.priority }}"
              - "area/{{ .Fields.area }}"
              - triaged
          - add: [needs-info]
            if: answer.fields.needsInfo
```

[`recipes/issue-triage/.kritik/tasks/triage.md.tmpl`](recipes/issue-triage/.kritik/tasks/triage.md.tmpl):

```gotemplate
Triage issue #{{ .Subject.Number }} of {{ .Repo.Owner }}/{{ .Repo.Name }}, opened by {{ .Subject.Author }}.

Answer with:
- priority: p0 for an outage, data loss or a security problem; p1 for a broken feature with no workaround;
  p2 for a bug with a workaround; p3 for everything else.
- area: the part of the project the issue is about.
- needsInfo: true when the report lacks what a maintainer needs to act on it, such as the version,
  the steps to reproduce or the logs; list what is missing in missing, most important first.
- labels: add the one type label that fits, and remove needs-triage.
- comment: when needsInfo is true, a short, polite request for what is missing, addressed to the author.
  When one of the possible duplicates below is clearly the same issue, say which. Otherwise leave it empty.

The contributing guide:
{{ .Context.files }}

The issues and pull requests most like this one, as possible duplicates:
{{ .Context.dupes }}
```

[`recipes/issue-triage/.kritik/tasks/triage-comment.md.tmpl`](recipes/issue-triage/.kritik/tasks/triage-comment.md.tmpl):

```gotemplate
Thanks for the report, @{{ .Subject.Author }}.
{{ with .Answer.Comment }}
{{ . }}
{{ end }}{{ if .Fields.needsInfo }}
To move this forward, please add:
{{ range .Fields.missing }}
- {{ . }}{{ end }}
{{ end }}{{ with .Applied.AddLabels }}
Labels added: {{ join ", " . }}{{ end }}
```

The repository needs the labels the task may add (`bug`, `enhancement`,
`question`, `documentation`, `needs-info`, `triaged` and each
`priority/…` and `area/…`); any it lacks is dropped from the run and noted.

#### Label pull requests by area

Offer every `area/` label the repository has, and add the ones the model
picks only when it says it is sure. A task sees a pull request's title and
description, not its diff.

[`recipes/pr-area-labels/.kritik.yaml`](recipes/pr-area-labels/.kritik.yaml):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/home-operations/kritik/main/docs/kritik.schema.json
tasks:
  - name: pr-area
    on:
      - pull_request: [opened, ready_for_review]
    if: "!subject.draft"
    mode: single
    promptInline: |
      Pick the areas of the project pull request #{{ .Subject.Number }} of {{ .Repo.Owner }}/{{ .Repo.Name }}
      changes, from its title and description, and add the matching area/ labels. Say how sure you are in confidence.
    fields:
      confidence: { type: string, enum: [low, high] }
    actions:
      labels:
        # Every area/ label the repository has is on offer, but none is
        # added unless the model is sure.
        propose: { add: ["area/*"] }
        if: answer.fields.confidence == "high"
```

#### Release summary

Summarize each published release for the dashboard. `release` is a raw
event, so the operator must allow it, with `raw:release.*` in
`allow.tasks.events`, and the task has no actions: the summary and its
fields are the run's report.

[`recipes/release-summary/.kritik.yaml`](recipes/release-summary/.kritik.yaml):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/home-operations/kritik/main/docs/kritik.schema.json
tasks:
  - name: release-summary
    # A forge-specific event; the operator must allow it, e.g. with
    # "raw:release.*" in allow.tasks.events.
    on:
      - raw: { event: release, actions: [published] }
    mode: single
    promptInline: |
      Summarize release {{ with .Raw.release }}{{ .tag_name }} {{ end }}of {{ .Repo.Owner }}/{{ .Repo.Name }} for its maintainers:
      what changed, whether anything breaks, and what users must do to upgrade. The release payload follows.
    fields:
      breaking: { type: boolean }
      highlights:
        type: array
        items: { type: string, maxLength: 200 }
        maxItems: 5
    # No actions: a release has no issue or pull request to write to, so the
    # summary and fields are the dashboard's report alone.
```

## Limits

A file that fails to parse, a bad `tasks` block included, is ignored as a
whole, and noted rather than failing the review. Every referenced file, plus `.kritik.yaml` itself, is
capped at 256 KiB, and 1 MiB in total; a file over either limit is skipped
and noted rather than failing the review.
