# Connecting a forge

kritik reviews pull requests on GitHub, through a GitHub App, and on
Forgejo, through a bot account. On both, events reach kritik through one
webhook per installation: the GitHub App's own webhook, or one webhook on
the Forgejo user or organization. No repository needs a webhook of its own,
and none needs a file: a [`.kritik.yaml`](repository-config.md) is
optional.

An installation is one entry under a tenant's `installations` in the
configuration file, or one added in the dashboard for a dashboard-managed
tenant. Its webhook address is kritik's webhook listener followed by
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
- **Where it can be installed:** only on this account. kritik serves only
  the installation's `account`; a delivery for any other account is
  accepted and ignored.

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
        account: example
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

Install the App on the account, for all repositories or selected ones.
Each pull request in them is reviewed when it opens and after each push,
under the tenant's settings.

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
        account: example
        token: { file: /var/run/secrets/kritik/bot/forgejo-token }
        gitToken: { file: /var/run/secrets/kritik/bot/forgejo-read-token }
        webhookSecret: { file: /var/run/secrets/kritik/bot/webhook-secret }
```

### Add one webhook

Add a Forgejo webhook to the organization that owns the repositories, or
to the user for repositories a user owns. It covers every repository the
owner has, including ones added later.

- **Target URL:** `https://<listener host>/hooks/<installation name>`,
  method POST. Either content type works.
- **Secret:** the installation's `webhookSecret`.
- **Trigger on:** custom events: Push, and under pull request events,
  Modification, Synchronized and Comments.

On Forgejo, kritik answers a mention in a pull request's conversation or
in a reply inside an existing code conversation. Forgejo sends no event for
a review's body or for code comments submitted with a review, so a mention
there goes unanswered.

## Check that it works

Both forges keep each webhook's recent deliveries with kritik's response:
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
- the index is not updated on pushes to the default branch;
- only repositories kritik already knows, from the configuration or an
  earlier event, are polled.
