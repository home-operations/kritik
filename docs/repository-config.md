# `.kritik.yaml` reference

A repository may commit an optional `.kritik.yaml` at its root to tune how
kritik reviews it. It is read from the merge-base commit, never the pull
request's own tree, so a pull request cannot use its own copy to weaken the
review applied to it. kritik reads it before the review starts, and applies
it to follow-ups (from the pull request's merge base) and to indexing (from
the commit indexed) too.

The file holds nothing secret: no field takes a credential, a URL, a host
or a secret reference, and it can only name what the operator configured,
a model by its `<provider>/<model>` reference and a command by its name.

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

## Limits

A file that fails to parse is ignored as a whole, and noted rather than
failing the review. Every referenced file, plus `.kritik.yaml` itself, is
capped at 256 KiB, and 1 MiB in total; a file over either limit is skipped
and noted rather than failing the review.
