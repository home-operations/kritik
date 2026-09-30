# ADR-0019: the configuration lives in git, and the dashboard reads it

- **Status:** Proposed
- **Date:** 2026-09-29
- **Authors:** onedr0p.
- **Amended by:** [ADR-0021](0021-configuration-shape.md), which lets
  the defaults and an `owner/*` entry say where repositories start (§2.3).
- **Supersedes:** [ADR-0015](0015-instance-defaults-in-the-file.md) (with
  no spec to lay them under, the file's instance defaults are simply the
  defaults) and [ADR-0012](0012-github-app-manifest.md) (the dashboard no
  longer registers an App).
- **Amends:** [ADR-0014](0014-github-app-only-self-hosted.md) §2.1, §2.2,
  §2.3 and §2.6 (the file holds the whole configuration, the instance
  spec, its sealing key and the setup wizard go, and the App is created
  by hand), and [ADR-0017](0017-dashboard-sections.md) §2.5 (Settings
  shows the configuration rather than editing it).

> Scope: where kritik's configuration is written and what the dashboard
> may still change. How a review runs, sign-in, and a repository's own
> `.kritik.yaml` do not change.

## 1. Context

ADR-0014 moved every setting but sign-in and the file's connections into
one row in Postgres, the instance spec, edited in the dashboard: its
providers and sealed keys, defaults, accounts, repositories, polling,
indexing, retention, egress, tools and embedder, with a setup wizard over
it and a manifest flow that registers the GitHub App. That shape came
from running kritik for others, where the dashboard is the only way in.

kritik is deployed by the people who run it, from git, with a chart
whose values already render a configuration file that every replica
re-reads. A second store for the same settings means two places to look
when a review behaves oddly, a sealing key to keep, revisions that
conflict, secrets entered by hand into a form rather than kept with the
cluster's other secrets, and about three thousand lines of dashboard and
API that exist only to edit what a pull request to the deployment
repository changes just as well.

## 2. Decision

### 2.1 The file holds the whole configuration

The configuration file takes every key the spec held: `providers`,
`defaults` (all of them, not ADR-0015's part), `polling`, `indexing`,
`tools`, `retention`, `egress`, `embedding`, `connections` and
`accounts`, beside `auth`. Secrets stay references, `{ env: NAME }` or
`{ file: path }`, to a Secret the deployment keeps as it keeps any other.
The environment variables of ADR-0014 §2.2 and ADR-0015 keep their keys
and their precedence over the file.

The file is loaded, validated as a whole, and re-read on the chart's
interval, as it is today; a change that does not validate is refused and
the last good configuration keeps running. `instance_config`, its
revisions and notifications, `KRITIK_DASHBOARD_KEY` and its keyring, and
`configsource`'s merge go.

A repository's `.kritik.yaml` is unchanged: it is the repository's own
configuration, in its own git.

### 2.2 The dashboard reads the configuration

The dashboard shows what runs and where each value comes from, and edits
none of it. What goes:

- the instance and account configuration editors, their JSON view,
  secret fields, provider and embedder key tests and the generated
  secrets dialog;
- the setup wizard, replaced by a checklist of what is missing (no
  connection, no review model, the App not installed on an account) with
  a link to the documentation for each;
- the manifest flow that registers a GitHub App: the App is created on
  GitHub by hand, as docs/setup.md describes, and its credentials go
  into a Secret;
- settings search entries that land on a form field.

Settings becomes the account's Repositories, its Audit log, and a
Configuration page for an admin: the running configuration as it was
loaded, each value with its source (the file, the environment or
kritik's own default), and why the file's latest content was refused,
when it was.

### 2.3 What the dashboard still changes

Actions on work, not on configuration, stay: re-running a review, one or
several at once, cancelling one, reindexing a repository, re-reading the
repositories an App reaches, and uninstalling the App from an account no
connection serves.

**Turning a repository on or off stays too, and the dashboard owns it.**
Repositories are still discovered: the App's installations register
every repository it reaches. Each repository gets a `turned_on` choice in
Postgres, unset until an admin turns it on or off from the Repositories
page, one at a time or several at once, and each change is audited like
an action. Whether a repository runs is decided in this order:

1. an archived repository never runs;
2. one an admin turned on or off runs as they chose;
3. a fork does not run;
4. any other runs as the configuration's `enabled` says, at the defaults
   or the account.

The file's `enabled` is therefore where a repository starts, not a switch
over it, and a repository entry in the file may not set it. A
repository's `.kritik.yaml` may still turn its own reviews off.

## 3. Consequences

- A change to how kritik reviews is a pull request to the deployment
  repository, reviewed and reverted like any other, and the cluster's
  secrets stay in one place.
- An instance runs with no database state beyond its work: restoring
  Postgres restores history and repository choices, not configuration.
- The one-owner rule of ADR-0010 holds again without exceptions: the
  file owns configuration, the dashboard owns a repository's on or off,
  and a repository's `.kritik.yaml` owns what it narrows.
- A new instance is set up by writing its file; the checklist says what
  it still lacks.
- kritik has not had a release; an instance with a stored spec copies it
  into its file once, by hand, before it runs this.

## 4. Rejected alternatives

- **Keep the editors and write the changes back to git.** kritik would
  hold credentials to push to the deployment repository and would race
  its own reconciler.
- **Keep the spec as an optional layer over the file.** Two owners for
  every setting is the problem this solves.
- **Put a repository's on or off in the file as well.** Two writers for
  one switch leave nobody sure which one won; the file sets where a
  repository starts instead.
