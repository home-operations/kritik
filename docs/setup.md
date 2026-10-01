# Setup

kritika reviews pull requests on github.com through a GitHub App. Events
reach kritika through the App's own webhook, which covers every repository
the App is installed on. No repository needs a file: a
[`.kritika.yaml`](repository-config.md) is optional. This guide takes a new
instance from install to its first review.

## Deploy

Install the chart as its [README](https://github.com/home-operations/kritika/blob/main/charts/kritika/README.md) shows, with:

- A Postgres database with VectorChord and kritika's three roles, under
  `database`: see [Postgres with CloudNativePG](database.md).
- `config.webUrl`, the one public URL. The dashboard is served at it, and GitHub
  delivers each App's webhook under it, to `/hooks/<app name>`, both from
  one port. The chart's `ingress` or `httpRoute` routes the URL there;
  nothing else needs to be public.
- A way to sign in: `KRITIKA_AUTH_ADMIN_PASSWORD` under `env`, from an
  existing Secret, for the local admin, or OIDC or GitHub with a role
  mapping that makes someone an admin ([`auth`](configuration.md#auth)).
- The [configuration file](configuration.md) as `configFile`, or an existing
  ConfigMap, with the variables its secrets name set from existing Secrets
  under `env` or `envFrom`. It lives in git with the rest of the
  deployment; kritika reads it at startup, and the chart rolls the pods
  when it changes.

A minimal file names the GitHub App (below), a model key and the default
review model:

```yaml
apps:
  github:
    accounts: [org-1]
    clientId: Iv1.example
    privateKey: { env: GITHUB_APP_PRIVATE_KEY }
    webhookSecret: { env: GITHUB_APP_WEBHOOK_SECRET }
providers:
  openrouter:
    type: openrouter
    apiKey: { env: OPENROUTER_API_KEY }
defaults:
  models: { review: openrouter/vendor/large-model }
```

Add `embedding` to index each repository for similar code, `repositories`
entries for an account's or a repository's own settings and rules, and
`accounts` for an account's limits
([configuration](configuration.md)).

## Sign in

Sign in as an admin. Until the instance can review, a banner says what is
missing and leads to the Configuration page, under Settings, whose Setup
checklist names each step and what to set for it
([first run](dashboard.md#first-run)).

## The GitHub App

An entry of `apps` is one GitHub App that kritika serves accounts through.
It serves the users and organizations its `accounts` lists, and each of
them is a kritika account, `github/<name>`, that this App alone serves.

### Register it

Register a GitHub App under the account whose repositories kritika reviews
(a personal account's or an organization's Developer settings):

- **Webhook:** Active, with the URL
  `<config.webUrl>/hooks/<app name>` and a random secret.
  This one webhook receives the events of every repository the App is
  installed on.
- **Repository permissions:**
  - Contents: read-only, for branches, files, comparisons and fetching the
    code.
  - Metadata: read-only.
  - Pull requests: read and write, for reviews, inline comments and
    replies, and conversation comments.
  - Issues: read-only. GitHub delivers a pull request's conversation
    comments as issue comments, and an App subscribes to those only with
    this permission.
  - Commit statuses: read and write, for the `kritika/review` status.
- **Organization permissions:** Members: read-only, only for signing in
  with GitHub through this App, whose role mapping reads the
  organizations a person belongs to.
- **Events:** Pull request, Pull request review comment, Issue comment,
  Push and Repository, which says when a repository is created, archived
  or unarchived. Installation events arrive without subscribing.
- **Where it can be installed:** only on this account, unless it should
  serve several. A public App can be installed on many organizations: list
  each one kritika should review in the App's `accounts`. A
  delivery for any account not listed is accepted and ignored, so nobody
  else who installs the App gets reviews.

Then generate a private key and note the App's client ID. Comments mention
the bot as `@<app slug>`, and only someone with write access gets an
answer. `@<app slug> review` queues a review of the pull request's head
instead of asking a question: a pull request from a fork is not reviewed on
its own, since its code comes from outside the organization, and this is
how a maintainer gets it one. `@<app slug> dismiss <reason>`, as a reply in
one of kritika's finding threads, dismisses that finding: its thread is
resolved, later reviews of the pull request are told not to raise it
again, and the dashboard lists it dismissed with the reason.
`@<app slug> pause` stops the pull request's automatic reviews, and
`@<app slug> resume` starts them again. Put the private key and the webhook secret
in a Secret, set a variable from each under `env`, and declare the
App under `apps` in the configuration file or the environment
([`apps`](configuration.md#apps)). To
sign in with GitHub through the same App, generate a client secret on its
settings page and set it, with the client ID, as the `KRITIKA_AUTH_GITHUB_*`
variables.

### Install it

Install the App on each account in `accounts`, for all repositories or
selected ones. Each pull request in them is reviewed when it opens and
after each push, under its settings in the configuration file, in every
repository that runs
([which repositories run](configuration.md#which-repositories-run)).

Anyone can install a public App by its slug. The Configuration page's
GitHub Apps panel lists every account each App is installed on, marks
those it does not serve, and uninstalls the App from any of them. kritika
reviews nothing on an account the App's entry does not list, whether or
not the App is installed there.

## Check that it works

GitHub keeps the App webhook's recent deliveries with kritika's response:
204 for a ping, 202 for anything accepted, 401 when the secrets differ or
the App has none, and 404 when the path names no App. The account
overview's Connection panel shows when its App last had a delivery,
explains where the webhook goes while none has, and says to set the App's
webhook secret when its deliveries arrive from GitHub with no signature,
which the Configuration page's GitHub Apps panel marks `unsigned`.
`kritika_webhooks_total{connection,outcome}` counts deliveries by outcome.

## Without webhooks

When the forge cannot reach the listener, polling alone still reviews:
every `KRITIKA_POLL_INTERVAL` (10 minutes unless set, `0s` turns it off), the
leader lists the open pull requests updated since the last poll. It is a
backstop, not a substitute:

- a review waits for the next poll;
- no mention is answered, since the poller does not read comments;
- the index catches up with the default branch at the next poll, not on
  each push: while no webhook has reached an App within
  `KRITIKA_POLL_LOOKBACK`, each poll also checks its indexed repositories'
  default branches;
- only repositories kritika already knows, from the configuration or an
  earlier event, are polled.
