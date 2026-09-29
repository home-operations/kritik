# Setup

kritik reviews pull requests on github.com through a GitHub App. Events
reach kritik through the App's own webhook, which covers every repository
the App is installed on. No repository needs a file: a
[`.kritik.yaml`](repository-config.md) is optional. This guide takes a new
instance from install to its first review.

## Deploy

Install the chart as its [README](../charts/kritik/README.md) shows, with:

- `web.url`, the one public URL. The dashboard is served at it, and GitHub
  delivers each connection's webhook under it, to
  `/hooks/<connection name>`. The chart's `ingress` or `httpRoute` routes
  `/hooks` to the webhook listener and everything else to the dashboard;
  nothing else needs to be public.
- `dashboard.keySecret`, the key that seals the secrets the dashboard
  keeps.
- A way to sign in: `auth.admin.passwordSecret` for the local admin, which
  is the way into a fresh instance, or OIDC or GitHub with a role mapping
  that makes someone an admin ([`auth`](configuration.md#auth)).

## Sign in, and follow the wizard

The first admin to sign in is met by the [setup wizard](dashboard.md#first-run),
which walks the rest of this guide: the GitHub App, a model key and review
model, the embedder, and the repositories to review. Every step can also
be done in the admin console.

## The GitHub App

A connection is one GitHub App that kritik serves accounts through. It
serves the users and organizations its `accounts` lists, and each of them
is a kritik account, `github/<name>`, that this connection alone serves.

### Create it from the admin console

The admin console's "Create a GitHub App" registers the App for you, from
a [manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
that sets its webhook, permissions and events:

1. Name the connection, and choose whether the App belongs to your own
   GitHub account or to an organization, and whether it is private or
   public.
2. "Create on GitHub" takes you to GitHub, which shows the App as kritik
   described it. Confirm it there within an hour.
3. GitHub sends you back to kritik, which adds the App as a dashboard
   connection serving the account it belongs to. The admin console then
   shows the App's client secret, once: kritik does not keep it. Set it
   and the client ID as `auth.github` to sign in with GitHub through the
   same App.
4. Install the App from the link the admin console shows.

To serve more accounts through a public App, add them to the connection's
`accounts` afterwards.

### Register it by hand

Register a GitHub App under the account whose repositories kritik reviews
(a personal account's or an organization's Developer settings):

- **Webhook:** Active, with the URL
  `<web.url>/hooks/<connection name>` and a random secret.
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
  - Commit statuses: read and write, for the `kritik/review` status.
- **Organization permissions:** Members: read-only, only for signing in
  with GitHub through this App, whose role mapping reads the
  organizations a person belongs to.
- **Events:** Pull request, Pull request review comment, Issue comment,
  Push and Repository, which says when a repository is created, archived
  or unarchived. Installation events arrive without subscribing.
- **Where it can be installed:** only on this account, unless it should
  serve several. A public App can be installed on many organizations: list
  each one kritik should review in the connection's `accounts`. A
  delivery for any account not listed is accepted and ignored, so nobody
  else who installs the App gets reviews.

Then generate a private key and note the App's client ID. Comments mention
the bot as `@<app slug>`, and only someone with write access gets an
answer. `@<app slug> review` queues a review of the pull request's head
instead of asking a question: a pull request from a fork is not reviewed on
its own, since its code comes from outside the organization, and this is
how a maintainer gets it one. Add the App as a connection in the admin console, whose "Add
connection" takes these three values and can generate the webhook secret,
or declare it in the configuration file or the environment
([`connections`](configuration.md#connections)).

### Install it

Install the App on each account in `accounts`, for all repositories or
selected ones. Each pull request in them is reviewed when it opens and
after each push, under the account's settings, which an admin sets on the
account's Configuration page, under Settings.

Anyone can install a public App by its slug. The admin console's
Connections panel lists every account each connection's App is installed
on, marks those the connection does not serve, and uninstalls the App
from any of them. kritik reviews nothing on an account its connection
does not list, whether or not the App is installed there.

## Check that it works

GitHub keeps the App webhook's recent deliveries with kritik's response:
204 for a ping, 202 for anything accepted, 401 when the secrets differ,
and 404 when the path names no connection. The account overview's
Connection panel shows when its connection last had a delivery, and
explains where the webhook goes while none has.
`kritik_webhooks_total{connection,outcome}` counts deliveries by outcome.

## Without webhooks

When the forge cannot reach the listener, polling alone still reviews:
every `polling.interval` (10 minutes unless set, `0s` turns it off), the
leader lists the open pull requests updated since the last poll. It is a
backstop, not a substitute:

- a review waits for the next poll;
- no mention is answered, since the poller does not read comments;
- the index catches up with the default branch at the next poll, not on
  each push: while no webhook has reached a connection within
  `polling.lookback`, each poll also checks its indexed repositories'
  default branches;
- only repositories kritik already knows, from the configuration or an
  earlier event, are polled.
