# ADR-0025: a review may approve the pull request

- **Status:** Proposed
- **Date:** 2026-09-30
- **Authors:** onedr0p.
- **Amends:** [ADR-0010](0010-configuration-layers.md) §2.5 (a second
  setting a repository may widen) and [ADR-0021](0021-configuration-shape.md)
  §2.1 (one more key in the shape).

> Scope: whether and when kritik submits an approving review, and where
> that is configured. What a review reports is unchanged.

## 1. Context

kritik informs and does not block: its commit status is `success` for
every review that ran, and its inline findings go out as a `COMMENT`
review, never an approval or a request for changes (ADR-0002). A
maintainer reads the summary and approves, or not.

Some repositories want the other half. Dependency bumps merge on
auto-merge behind a branch rule that requires an approval, and a
maintainer approving a Renovate pull request that kritik already read is
a rubber stamp. The verdict is already in the summary: an approval would
only restate it where GitHub's merge rules can see it.

## 2. Decision

### 2.1 Approve on nothing blocking or important

With `approve: true`, a review that reached a verdict and found nothing
of severity `blocking` or `important` submits an `APPROVE` review pinned
to the head it reviewed, with a one-line body naming the head. Nits do
not withhold it: they are the findings a maintainer may leave as they
are. A review that did not reach a verdict (skipped, incomplete, failed,
canceled) never approves.

An incremental review restates the prior findings that still hold, so
its result is the verdict for its head, and the same rule applies: a
push that resolves an important finding is approved on its review, and
one that introduces one is not.

A head the bot already approved is not approved again, so a re-run is
idempotent.

### 2.2 Withdraw when the verdict changes

A review that does find something blocking or important dismisses each
of the bot's standing approvals of the pull request, with a message
giving the counts. An approval never outlives the verdict behind it, and
never turns into a request for changes: kritik still does not block.

GitHub only lets the App dismiss where its permission allows; on a
protected branch that lists who may dismiss, the dismissal can fail,
which is logged like a failed commit status. A branch rule that
dismisses stale approvals on push covers the same case on the forge's
side.

### 2.3 Best effort, like the status

The approval and the dismissal follow the sticky comment, the inline
review and the commit status, and fail the same way: logged, with the
review published. A forge that refuses the approval cannot turn a
finished review into a retry.

### 2.4 Configured as `approve`, a plain inherited boolean

`approve` joins the review keys the configuration's `defaults`, `owner/*`
and `owner/name` entries and a repository's `.kritik.yaml` take (ADR-0021
§2.1), off unless set. The narrowest scope that sets it wins, in either
direction: `defaults: { approve: true }` approves everywhere but where an
entry or a file says `approve: false`, and a file's `approve: true` turns
it on for a repository where the instance leaves it off.

That is a second setting a repository may widen, after
`requireSuggestedFix` (ADR-0010 §2.5), and the justification is the same
one ADR-0010 gives for the `allow` block: the file is read from the merge
base, so a pull request cannot enable an approval for itself, and turning
it on takes a change to the base branch, by the people who already write
the review's instructions and rules. An operator who wants it off
everywhere sets it nowhere; there is no bound to hold it off against a
repository, which is the design chosen for this fleet, where a
repository's reviewers would see the file change.

## 3. Consequences

- **The App's approval counts** toward a branch rule's required
  approvals, which is the point; it does not satisfy a rule that
  requires a review from a person, and GitHub does not let an App
  approve a pull request the same App opened.
- **The App needs no new permission.** The pull requests write permission
  the inline review already needs covers submitting and dismissing a
  review.
- **A re-review after a fix approves**, and a review that finds a
  regression withdraws, so the approval tracks the head. What it cannot
  track is a change kritik did not review: a skipped head keeps whatever
  stood, and the branch rule decides whether a stale approval counts.
- **The dashboard** lists `approve` with the other review settings and
  where it comes from.
