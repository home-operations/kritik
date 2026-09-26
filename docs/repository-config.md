# `.kritik.yaml` reference

A repository may commit an optional `.kritik.yaml` at its root to narrow how
kritik reviews it. It is read from the merge-base commit, never the pull
request's own tree, so a pull request cannot use its own copy to weaken the
review applied to it; a file that fails to parse is ignored as a whole, and
noted rather than failing the review. It can only narrow what the operator
already allows — `mode`, `agent`, `incremental` and `settle` stay
operator-only — and its keys are:

- `enabled: false` — disables review for the repository (it cannot turn a
  disabled repository back on).
- `filter` — a filter expression ANDed with the operator's own; it is
  compiled and smoke-tested against a sample pull request when the file is
  parsed, so a broken expression is rejected rather than silently skipping
  every review.
- `ignore` — path globs added to the operator's own ignore list.
- `skip.onlyPaths` — path globs; the pull request is skipped only when
  every changed path matches at least one of them.
- `review.instructions` — paths to files (read from the same merge-base
  tree) appended to the reviewer's system prompt, capped at 32 KiB joined.
- `review.requireSuggestedFix` — whether findings must include a suggested
  fix.
- `review.templates.summary` / `review.templates.inline` — paths to Go
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
  `.Incremental`, `.PriorHeadSHA`, `.Incomplete`); the inline template's dot
  is one finding (`.Path`, `.Line`, `.EndLine`, `.Severity`, `.Title`,
  `.Explanation`, `.SuggestedFix`, `.Replacement`, `.AgentPrompt`, `.URL`, a
  link to the lines at the head commit). Rendering is bounded (loop iterations, bytes per
  function call, output size, a deadline) so a template cannot hang or
  exhaust memory; one that exceeds a bound falls back to the default with a
  note in the comment.

Every referenced file, plus `.kritik.yaml` itself, is capped at 256 KiB,
and 1 MiB in total; a file over either limit is skipped and noted rather
than failing the review.
