# `.kritik.yaml` reference

A repository may commit an optional `.kritik.yaml` at its root to tune how
kritik reviews it. It is read from the merge-base commit, never the pull
request's own tree, so a pull request cannot use its own copy to weaken the
review applied to it. kritik reads it before the review starts, and applies
it to follow-ups (from the pull request's merge base) and to indexing (from
the commit indexed) too.

The file holds nothing secret: no field takes a credential, a URL, a host
or a secret reference, and a model it names is a `<provider>/<model>` of a
provider an admin configured.

[`kritik.schema.json`](kritik.schema.json) is its JSON Schema. An editor
using the YAML language server validates the file as it is written when
its first line names the schema:

```yaml
# yaml-language-server: $schema=https://kritik.home-operations.com/kritik.schema.json
```

## What it may set

The file takes the review keys the configuration's `defaults` and
repository entries take, at its top level. It narrows what an
admin allows, adds to the review's rules and context, and replaces the
rest:

```yaml
models: { review: openrouter/anthropic/claude-opus-5.5 }
feedback: standard
comments: { inline: true }
filterExpr: "!pr.draft"
ignore: ["web/src/generated/**", "docs/**"]
rules:
  - {
      id: wrap-errors,
      rule: 'Wrap errors with fmt.Errorf("<package>: %w", err).',
      paths: ["**/*.go"],
    }
  - { id: house-style, file: .kritik/review.md }
  - id: renovate
    rule: Say what the update breaks, from the release notes in the body.
    whenExpr: pr.headRef.startsWith("renovate/")
context:
  - { path: ARCHITECTURE.md, description: how the services fit together }
```

- `enabled: false`: stops reviews, follow-ups and indexing for the
  repository. It cannot turn a disabled repository back on.
- `models.review` / `models.fallback`: a `<provider>/<model>` of a
  provider the instance or the repository's account declares, used for
  the review and for follow-ups. A model of any other provider is
  dropped; the account's limits bound what a choice can cost. A fallback
  applies only on the review model's provider.
- `feedback`: how much the review says, replacing the
  admin's.

  | `feedback`           | What the review reports                                                                                                                                                                |
  | -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
  | `detailed` (default) | every line a maintainer could act on, smaller improvements, missing tests and questions included, each inline, with a one-click suggestion wherever the fix is a change to those lines |
  | `standard`           | the same review, with nits in the summary rather than inline                                                                                                                           |
  | `minimal`            | only what would stop the review: bugs, risks and breaking changes                                                                                                                      |

  A `blocking` finding is always posted inline, and the summary lists
  every finding.

- `comments.inline: false`: posts the summary alone, without inline
  comments.
- `comments.summaryTemplate` / `comments.inlineTemplate`: paths to Go
  [text/template](https://pkg.go.dev/text/template) templates that replace
  kritik's built-in summary and inline comment templates; an empty path
  restores the built-in one where the admin set a template. They use the
  [sprout](https://github.com/go-sprout/sprout) helpers tuppr and chaski
  expose (std, strings, conversion, encoding, numeric, slices, maps, regex,
  time, semver and reflect; not env, filesystem, network, random, uniqueid
  or checksum, and not `set` or `unset`). The `template`, `define` and
  `block` actions are refused, so a template cannot read any file or call
  any other template. The summary template's dot is the review (`.Number`,
  `.HeadSHA`, `.HeadURL`, `.Model`, `.AuthorIsBot`, `.Result.Summary.Take`,
  `.Result.Summary.Praise`, `.Result.Findings`,
  `.Counts.Blocking`/`.Important`/`.Nit`, `.Unanchored`, the findings on
  lines the diff does not show, `.Notes`, `.Incremental`, `.PriorHeadSHA`,
  `.PriorHeadURL`, `.Prior`, the last review's findings each with
  `.Resolved`, `.Sources` and `.Incomplete`). The inline template's dot is
  one finding (`.Path`, `.Line`, `.EndLine`, `.Severity`, `.Title`,
  `.Explanation`, `.SuggestedFix`, `.Replacement`, `.AgentPrompt`, `.Rules`,
  the ids of the rules it enforces, `.URL`, a link to the lines at the head
  commit, and `.ThreadURL`, a link to its inline comment thread once one
  is posted). Rendering is bounded (loop iterations, bytes per function
  call, output size, a deadline), so a template cannot hang or exhaust
  memory; one that exceeds a bound falls back to the default with a note
  in the comment.
- `requireSuggestedFix: true`: findings must include a suggested fix. The
  file can turn the requirement on, never off.
- `approve: true`: a review that finds nothing blocking or important
  approves the pull request, as a review pinned to the head it saw; nits
  alone do not withhold it. A later review of the same pull request that
  does find something dismisses kritik's approval. It replaces the
  admin's, in either direction: a repository turns it on where the
  instance leaves it off. Off unless
  set.
- `filterExpr`: a filter expression ANDed with the admin's own. It is
  compiled and smoke-tested against a sample pull request when the file is
  parsed, so a broken expression is rejected rather than silently skipping
  every review. A review it filters out ends before any runner starts.
- `ignore`: path globs added to the admin's own ignore list, for
  reviews and indexing alike. A pull request whose every changed path is
  ignored, by these, the admin's globs or kritik's defaults (vendored
  trees and lockfiles), is skipped.
- `rules`: checks the review makes, added after the admin's. Each has an `id` (lowercase letters,
  digits and hyphens, at most 64 characters) that findings cite it by,
  and either the `rule` itself (at most 2000 characters) or a `file`,
  read from the same merge-base tree, whose content is the check; optional `paths`
  globs apply it only when a changed path matches one, so checks for one
  part of the repository do not spend the room on changes elsewhere. An
  optional `whenExpr`, a CEL expression over the `pr` that `filterExpr`
  sees, applies it only to a pull request it is true of, such as
  `pr.headRef.startsWith("renovate/")` for Renovate's; it is compiled and
  smoke-tested like `filterExpr`, and a rule whose `whenExpr` fails to
  evaluate is left out. A rule whose `id` an admin's rule has is
  dropped, and the review's summary says so. The rules a change matches
  are listed by id in the system prompt (and a follow-up's), a file rule
  under a heading of its own, within 16 KiB of rule text and 32 KiB of
  rule files, and a finding lists the ids of the rules it enforces,
  keeping only ones its review was given.
- `context`: files that explain the code, each a `path` with a
  `description` and optional `paths` globs, added after the admin's. The
  review is pointed at each file to read it with its own tools. A file
  with `paths` applies only when a changed path matches one of them.
- `agentFiles: false`: leaves the repository's agent files out. Unless
  set, a review adds to its instructions the `AGENTS.md` of the root and
  of each directory above a changed path, or a directory's `CLAUDE.md`
  where it has no `AGENTS.md`, read from the merge base, within 32 KiB. They follow the rules in the
  prompt.

A value the file may not take, such as an unknown feedback level or a
model of an undeclared provider, is dropped: the admin's value applies for
that field, a note in the review's summary says which field was dropped
and what it may be, and the rest of the file still applies. `agent`,
`settle`, `forks`, `incremental`, `limits` and `runner` are the admin's
alone; a file naming one of them, or any other unknown key, does not
parse.

## `filterExpr` recipes

`filterExpr`, like a rule's `whenExpr`, is a [CEL](https://cel.dev)
expression over `pr`, which has the pull request's `number`, `title`,
`body`, `author`, `state`, `open`, `merged`, `draft`, `fork`, `headRef`,
`headSha`, `baseRef`, `url`, `createdAt` and `labels` (each with a `name`
and a `color`), and `event`, what started the review: `opened`,
`reopened`, `ready_for_review`, `synchronize` (a push), `poll` (a push
kritik found without its webhook) or `manual` (a re-run from the
dashboard).

Some filters, each the whole `filterExpr` value:

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
