# ADR-0017: the dashboard's sections

- **Status:** Proposed
- **Date:** 2026-09-29
- **Authors:** onedr0p.
- **Amends:** [ADR-0009](0009-web-dashboard.md) §2.1 (the layout copied
  from konflate) and [ADR-0016](0016-dashboard-identity.md)'s scope, which
  left that layout as it was.

> Scope: how the dashboard is organised: its sections, what each holds,
> how settings are laid out, and the few facts its pages report that
> kritik did not derive before. How a review runs does not change.

## 1. Context

The dashboard grew a page at a time with the service: a sidebar of seven
account pages, All accounts and an Admin console. Its order follows
kritik's parts, the queue, follow-ups, usage and transcripts, rather than
what a maintainer asks of a review bot: is it catching things worth
fixing, what did it say on this pull request, and how is it tuned. An
account's admin page and the admin console were two entries both called
Admin.

Greptile, a hosted review bot, organises its dashboard around those
questions. An audit of it on 2026-09-29 found four sections: Analytics
(reviews, bugs caught, time to merge, the share of comments addressed,
comment ratings), Memory (rules with their scope, usage and addressed
rate), Pull requests, and Settings (a searchable navigation of settings
laid out as rows). Much of it does not apply to a self-hosted GitHub App
([ADR-0014](0014-github-app-only-self-hosted.md)): billing, credits and
plans, seats and invitations, teams, referrals, enterprise upsells, other
forges, an integrations marketplace, API keys, and merging or closing a
pull request from the dashboard. Some of it kritik already does better:
Greptile has no page for a review or a repository and shows no index
status, ships one theme, and saves each setting the moment it changes,
with no undo.

## 2. Decision

### 2.1 A top bar with section tabs

The sidebar goes. The top bar holds the brand, an account switcher (every
account the viewer can read, and All accounts) and the actions; under it
is one tab per section: **Analytics**, **Pull requests**, **Rules** and
**Settings**. The current tab carries the highlighter. A section's list
pages share a strip of sub-tabs in place of a page title, while a
record's page, a pull request or a review, keeps its own title.

### 2.2 Analytics

The account's home. Its sub-tabs:

- **Reviews:** pull requests reviewed, reviews, findings by severity,
  addressed rate and spend over a period, each against the period
  before; reviews and findings by day; the repositories with the most;
  and the pull requests that need attention.
- **Findings:** every finding, once per pull request however many reviews
  repeated it, with its severity, pull request and whether it was
  addressed, searchable, linking to the review and to its thread on
  GitHub.
- **Spend:** the usage page.

### 2.3 Pull requests

A table, searched with one box that also takes filter tokens (`repo:`,
`author:`, `status:`) and suggests them, beside a switch between open,
closed and all pull requests. Each row is the pull request's title and
repository, its last review's status as an icon and a word, and its
findings by severity. Queue and Follow-ups are its sub-tabs. A pull
request and a review keep their pages in the dashboard, the diff with
each finding on its line included, and each finding links to its thread
on GitHub.

### 2.4 Rules

Every review instruction and context file each repository's reviews read,
with the layer that sets it (the instance, the account, the repository's
entry or its `.kritik.yaml`) and the paths it applies to. The page is
read-only: rules stay files in the repository, named by the
configuration.

### 2.5 Settings

A navigation of its own, filtered by typing: the account's Repositories,
Configuration and Audit log, and, for an admin, the instance's pages. A
setting is a row, its name and what it does on the left and its control
on the right: a segmented control for a choice of a few, and a line under
it saying what the current choice does. Settings are saved on request,
not as they change, since a save is checked against the revision it was
loaded at and audited; a bar says when there are unsaved changes.

### 2.6 Feedback

- **Addressed.** An incremental review re-checks each finding of the
  last one and reports it again only while it is still present, so a
  finding that a later completed review of the pull request no longer
  reports was addressed. This is derived when read; nothing new is
  stored.
- **Reactions.** The 👍 and 👎 on each inline comment kritik posted, read
  by the poller while its pull request is open.
- **Time to merge.** When a pull request was merged or closed, from its
  webhook or the poll.

### 2.7 Visual discipline

Colour marks severity, and whether a change between periods is good or
bad, which is not always its sign. Charts are ink and greys. A review's
status is an icon and a word rather than a coloured pill, and each metric
says what it counts. ADR-0016's type, palette, highlighter and sentence
case stay, as do both themes.

## 3. Consequences

- The routes and their hashes stay; each belongs to one section
  (`internal/web/src/lib/sections.ts`), and new pages add routes.
- The account overview becomes Analytics, and the Usage page its Spend
  sub-tab. Queue and Follow-ups stop being top-level pages.
- The findings list and analytics need read endpoints of their own;
  reactions and merge times need columns, added to the schema in place
  while kritik is unreleased.
- Every UI test that found its way through the sidebar goes through the
  tabs, the account menu or the settings navigation instead.

## 4. Rejected alternatives

- **Keep the sidebar and reorder it.** With four sections a sidebar is
  mostly empty, and tabs give the page its full width.
- **Save each setting as it changes, as Greptile does.** A save is
  revision-checked and audited, and a stray click on a setting that
  changes spend should not apply at once.
- **Greptile's uppercase monospace labels.** ADR-0016 chose sentence
  case.
- **Link every review out to GitHub instead of showing it.** The diff
  with findings on their lines, the transcript and the cost are what
  GitHub cannot show.
- **Merge, close and re-review keys on the pull request list.** kritik
  reviews pull requests and does not change their state; re-running
  stays a button with a confirmation.
