# Connecting a forge

kritik reviews pull requests on GitHub, through a GitHub App, and on
GitLab, Forgejo and Gitea, through a bot account. Events reach kritik
through the GitHub App's own webhook, one webhook on each GitLab group or
project, or one on each Forgejo or Gitea user or organization served. None
needs a file: a [`.kritik.yaml`](repository-config.md) is optional.

An installation is one entry under a tenant's `installations` in the
configuration file, or one added in the dashboard for a dashboard-managed
tenant, serving the users and organizations its `accounts` lists. Its
webhook address is kritik's webhook listener followed by
`/hooks/<installation name>`.

## Expose the webhook listener

The forge has to reach `POST /hooks/<installation>` on the listener, the
chart's `service.port` (8080) on `all` and `ingest` pods. The chart's
`httpRoute` or `ingress` values publish it, matching `/hooks` by default;
nothing else needs to be public for webhooks. The dashboard has its own
route (`httpRoute.web`, `ingress.web`).

## GitHub

### Register the App

Register a GitHub App under the account whose repositories kritik reviews
(a personal account's or an organization's Developer settings):

- **Webhook:** Active, with the URL
  `https://<listener host>/hooks/<installation name>` and a random secret.
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
- **Events:** Pull request, Pull request review comment, Issue comment and
  Push. Installation events arrive without subscribing.
- **Where it can be installed:** only on this account, unless it should
  serve several. A public App can be installed on many organizations: list
  each one kritik should review in the installation's `accounts`. A
  delivery for any account not listed is accepted and ignored, so nobody
  else who installs the App gets reviews.

Then generate a private key and note the App's client ID. Comments mention
the bot as `@<app slug>`, and only someone with write access gets an
answer.

### Configure the installation

```yaml
tenants:
  - slug: example
    installations:
      - name: example-github
        forge: github
        accounts: [example]
        app:
          clientId: Iv1.example
          privateKey: { file: /var/run/secrets/kritik/bot/private-key.pem }
          webhookSecret: { file: /var/run/secrets/kritik/bot/webhook-secret }
```

`webhookSecret` holds the same value as the App's webhook secret. In the
dashboard, a dashboard-managed tenant's installation form takes the same
three values, and can generate the webhook secret for you to copy into the
App.

### Install the App

Install the App on each account in `accounts`, for all repositories or
selected ones. Each pull request in them is reviewed when it opens and
after each push, under the tenant's settings.

## GitLab

GitLab calls a pull request a merge request; kritik reviews them as it
does pull requests elsewhere.

### The bot account

kritik posts reviews, comments and statuses as the user behind its token:

- a group or project access token, whose bot user GitLab creates. On
  GitLab.com these need Premium or Ultimate; on GitLab Self-Managed, any
  license.
- a personal access token of a user made for kritik, on any tier.

People mention the bot by that user's username. A group or project access
token's bot user is named like `group_<id>_bot_<random>`, which is harder
to type than a user named for kritik.

The token needs the `api` scope, and its user the Developer role on the
projects it reviews, since GitLab lets only Developers and above set a
commit status. A group access token, or membership of the group, covers
every project in it. Before answering a mention, kritik checks that the
commenter has at least the Developer role, which any member can read, so
no higher role is needed.

A second token with `read_repository` alone, given as `gitToken`, is what
runner pods fetch with, so the token that can write never enters the pod
that reads untrusted merge request content.

### Configure the installation

```yaml
tenants:
  - slug: example
    installations:
      - name: example-gitlab
        forge: gitlab
        # host: gitlab.example.com on GitLab Self-Managed; gitlab.com when unset.
        accounts: [example]
        token: { file: /var/run/secrets/kritik/bot/gitlab-token }
        gitToken: { file: /var/run/secrets/kritik/bot/gitlab-read-token }
        webhookSecret: { file: /var/run/secrets/kritik/bot/webhook-secret }
```

`accounts` lists top-level groups and users: a project in a subgroup
belongs to its top-level group.

### Add the webhook

Add one webhook to each group in the installation's `accounts`, which
covers every project in the group and its subgroups, including ones added
later. Group webhooks need Premium or Ultimate and the Owner role; without
them, add the webhook to each project instead, which needs the Maintainer
role.

- **URL:** `https://<listener host>/hooks/<installation name>`.
- **Secret token:** the installation's `webhookSecret`.
- **Trigger:** push, comment and merge request events.

kritik answers a mention in a merge request's overview or in a thread on
its diff. It never answers an internal note, since its answer would be
public. Anything it posts has its quick actions escaped, so a line such as
`/merge` in a review is shown, never run.

## Forgejo

### The bot account

Create a user for kritik and give it admin access to the repositories, for
example through an organization team. kritik posts reviews and statuses as
this user, and before answering a mention it checks the commenter's
permission, which Forgejo reveals only to repository and site admins:
without admin access, reviews still run, but no mention is answered.

Create an access token for it with these scopes:

- `read:user`, to learn its own login.
- `write:repository`, for pull requests, reviews, statuses and files.
- `write:issue`, for conversation comments.

A second token with `read:repository` alone, given as `gitToken`, is what
runner pods fetch with, so the token that can write never enters the pod
that reads untrusted pull request content.

### Configure the installation

```yaml
tenants:
  - slug: example
    installations:
      - name: example-forgejo
        forge: forgejo
        host: forge.example.com
        accounts: [example]
        token: { file: /var/run/secrets/kritik/bot/forgejo-token }
        gitToken: { file: /var/run/secrets/kritik/bot/forgejo-read-token }
        webhookSecret: { file: /var/run/secrets/kritik/bot/webhook-secret }
```

### Add one webhook per account

Add a Forgejo webhook to each organization, or user, in the
installation's `accounts`. Each covers every repository its owner has,
including ones added later.

- **Target URL:** `https://<listener host>/hooks/<installation name>`,
  method POST. Either content type works.
- **Secret:** the installation's `webhookSecret`.
- **Trigger on:** custom events: Push, and under pull request events,
  Modification, Synchronized and Comments.

On Forgejo, kritik answers a mention in a pull request's conversation or
in a reply inside an existing code conversation. Forgejo sends no event for
a review's body or for code comments submitted with a review, so a mention
there goes unanswered.

## Gitea

A Gitea installation is set up as a Forgejo one is, with `forge: gitea`:
kritik talks to both through the same client. The bot account, the token
scopes and the admin access that answering a mention needs are the same,
and so is one webhook on each account in `accounts`. Gitea's webhook form
names the events differently: under custom events, choose Push, and under
pull request events, Pull Request, Pull Request Synchronized and Pull
Request Comment.

## Check that it works

Every forge keeps each webhook's recent deliveries with kritik's response:
204 for GitHub's ping, 202 for anything accepted, 401 when the secrets
differ, and 404 when the path names no installation. The tenant overview's
Installations panel shows when each installation last had a delivery, and
explains where its webhook goes while none has.
`kritik_webhooks_total{installation,outcome}` counts deliveries by outcome.

## Without webhooks

When the forge cannot reach the listener, polling alone still reviews:
every `polling.interval` (10 minutes unless set, `0s` turns it off), the
leader lists the open pull requests updated since the last poll. It is a
backstop, not a substitute:

- a review waits for the next poll;
- no mention is answered, since the poller does not read comments;
- the index catches up with the default branch at the next poll, not on
  each push: while no webhook has reached an installation within
  `polling.lookback`, each poll also checks its indexed repositories'
  default branches;
- only repositories kritik already knows, from the configuration or an
  earlier event, are polled.
