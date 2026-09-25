# ADR-0006: a finding carries its fix as a suggestion and as an agent prompt

- **Status:** Proposed
- **Date:** 2026-09-25
- **Amends:** [ADR-0003](0003-forgejo-agentic-review.md) §2.4, the findings
  contract and the inline comment it renders to.
- **Authors:** onedr0p.

## 1. Context

A finding has a `suggested_fix`: free Markdown, replacement code or an
instruction, which the inline comment shows under a heading. The reader
then applies it by hand. GitHub and Forgejo render a `suggestion` fence in
a review comment as a change the author applies with one click, and
authors increasingly hand a review comment to a coding agent; both want
the fix in a shape the free text does not give. Mira, the most-used
self-hosted reviewer, posts both and its comment format was the prompt for
this change.

## 2. Decision

The contract gains three optional fields on a finding, and the forge
interface what they need:

- `end_line`: the last line of the range the finding covers, omitted when
  it covers `line` alone. Every line of the range must be one the diff
  adds or keeps; otherwise the range and the replacement are cleared and
  the finding kept.
- `replacement`: what lines `line` through `end_line` should read instead,
  raw code the model is told to write exactly as it should be committed.
  Fence lines a model wraps it in are stripped. The inline comment puts it
  in a `suggestion` fence, and the review comment spans the range on a
  forge whose comments can (`LineRanges`: GitHub); on one whose comments
  sit on a single line (Forgejo) a multi-line replacement is shown as a
  code block in the suggested fix instead, since the forge would apply it
  to one line. A replacement satisfies `requireSuggestedFix`.
- `agent_prompt`: one plain-text paragraph telling a coding agent what to
  change, rendered in a collapsed block under the comment inside a fence
  longer than any backtick run it contains.

The sticky comment links each finding to its lines at the head commit
(`FileURL` on the forge) and states the counts on one line rather than
three. Findings persist the new fields; a re-review's prior findings carry
them but the prompt lists findings as before.

## 3. Consequences

**Positive.** A fix the model gets right is applied in one click; one it
gets nearly right is a prompt away. The reader clicks from the summary to
the code. The change is additive: templates and repositories that ignore
the fields see the comments they saw before, with the count line as the
one visible difference.

**Negative.** A wrong replacement is easier to apply than a wrong
instruction, which is why the prompt insists it be complete and exactly as
committed and why the range is clamped to the diff. The prompt grows by a
few sentences, and an answer by the replacement's length.

## 4. Alternatives considered

- **Ask the model for a unified diff.** More general, but forges do not
  render diffs as suggestions, and models produce malformed hunks far more
  often than they produce wrong lines.
- **Derive the replacement from `suggested_fix`.** A code block in free
  text says nothing about which lines it replaces.
