# ADR-0018: rules written in the configuration

- **Status:** Proposed
- **Date:** 2026-09-29
- **Authors:** onedr0p.
- **Amends:** [ADR-0010](0010-configuration-layers.md) §2.4 (a key that
  adds up across scopes) and [ADR-0017](0017-dashboard-sections.md) §2.4
  (the Rules page lists written rules).
- **Amended by:** [ADR-0021](0021-configuration-shape.md), which lets a
  rule be a file and drops `review.instructions`.

> Scope: review rules as short statements in the configuration, how they
> reach a review, and how findings cite them. Instruction and context
> files are unchanged.

## 1. Context

A review follows what it is given: the built-in prompt, the instruction
files `review.instructions` names, and the context files
`review.context` names. Each is a file in the repository, so a maintainer
who wants one more check ("wrap errors with the package name", "never log
a token") writes a file, commits it and names it, and nobody can tell
afterwards whether the check ever caught anything. The Rules page of
ADR-0017 §2.4 lists those files, but can only point at them.

Greptile's rules are the model: a line of text, scoped to some paths,
written where the reviews are configured, and counted by the findings
that follow it.

## 2. Decision

### 2.1 The key

`review.rules` is a list at the defaults, an account, a repository entry
and a repository's `.kritik.yaml`:

```yaml
review:
  rules:
    - id: wrap-errors
      rule: 'Wrap an error with fmt.Errorf("<package>: %w", err) before returning it.'
      paths: ["**/*.go"]
```

- `id` names the rule to findings (§2.4) and to narrower scopes (§2.2):
  lowercase letters, digits and hyphens, starting with a letter or digit,
  at most 64 characters, once per list. Rewording a rule under the same
  id keeps its history.
- `rule` is the check, at most 2000 characters: what to look for and why,
  and an example when it helps.
- `paths`, optional, are globs: the rule applies only to a change that
  touches a matching path.

### 2.2 Layers

Rules add up, unlike every other key of ADR-0010 §2.4, where a narrower
scope replaces the broader one's value. The defaults' rules come first,
then the account's, then its repository entry's; a narrower scope's rule
with an id already listed replaces that rule where it stands, so a
repository can reword or rescope a rule the account sets. A
`.kritik.yaml` adds its own rules after the admin's but may not replace
one: a rule whose id an admin's rule has is dropped, with a note, since
the file never loosens what an admin sets (ADR-0010 §2.5).

### 2.3 The prompt

The rules whose `paths` match the change, or that have none, are listed
by id in the system prompt ahead of the repository instructions, for
single and agentic reviews and for follow-ups alike. They are capped at
16 KiB; the rules past the cap are left out, and the review's notes say
how many.

### 2.4 Citations

A finding lists the ids of the rules it enforces, in a `rules` field of
the finding contract, and kritik keeps only ids the review was given.
Each rule then has a count of the findings that cite it and how many of
those were addressed (ADR-0017 §2.3's derivation), over the same window
as the Findings page.

### 2.5 The Rules page

Rules are listed with the instruction and context files, their text
shown, each with its source, the paths it applies to, the repositories
that run it and its counts. The page stays read-only: a rule is written
where the rest of the configuration is.

## 3. Consequences

- A check is a line in the configuration rather than a file of its own
  and a reference to it; files stay the way to give a review long
  guidance.
- A noisy rule shows as one with many findings and few addressed, and a
  dead one as one with none.
- The finding contract gains a field; kritik has not had a release that
  would make this costly.

## 4. Rejected alternatives

- **Rules replace across scopes, as instructions do.** An account rule
  would silently drop every default rule, and a repository entry every
  account rule, which is not what adding one check means.
- **Rules as files only, with an id per file.** A count per file says
  little when one file holds twenty checks.
- **A switch to turn a rule off per repository.** A repository entry can
  narrow a rule's `paths` already; turning one off outright waits for a
  need.
