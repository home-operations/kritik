# Connecting a forge

kritik reviews pull requests on github.com through a GitHub App. Events
reach kritik through the App's own webhook, which covers every repository
the App is installed on. No repository needs a file: a
[`.kritik.yaml`](repository-config.md) is optional.

A connection is one GitHub App that kritik serves accounts through. It
serves the users and organizations its `accounts` lists, and each of them
is a kritik account, `github/<name>`, that this connection alone serves.
Its webhook address is kritik's webhook listener followed by
`/hooks/<connection name>`. A connection is declared in one of three
places:

- the configuration file's `connections`;
- the `KRITIK_CONNECTIONS_*` environment variables, which declare one;
- the admin console, which keeps it in the instance configuration in
  Postgres.

## Expose the webhook listener

The forge has to reach `POST /hooks/<connection>` on the listener, the
chart's `service.port` (8080) on `all` and `ingest` pods. The chart's
`httpRoute` or `ingress` values publish it, matching `/hooks` by default;
nothing else needs to be public for webhooks. The dashboard has its own
route (`httpRoute.web`, `ingress.web`).

## GitHub

### Register the App

Register a GitHub App under the account whose repositories kritik reviews
(a personal account's or an organization's Developer settings):

- **Webhook:** Active, with the URL
  `https://<listener host>/hooks/<connection name>` and a random secret.
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
  each one kritik should review in the connection's `accounts`. A
  delivery for any account not listed is accepted and ignored, so nobody
  else who installs the App gets reviews.

Then generate a private key and note the App's client ID. Comments mention
the bot as `@<app slug>`, and only someone with write access gets an
answer.

### Configure the connection

```yaml
connections:
  - name: github
    forge: github
    accounts: [org-1, user-1]
    app:
      clientId: Iv1.example
      privateKey: { file: /var/run/secrets/kritik/bot/private-key.pem }
      webhookSecret: { file: /var/run/secrets/kritik/bot/webhook-secret }
```

`webhookSecret` holds the same value as the App's webhook secret. The
same connection can come from the environment instead:

| Variable                                       | Key                                   |
| ---------------------------------------------- | ------------------------------------- |
| `KRITIK_CONNECTIONS_NAME`                      | `name`, `github` unless set           |
| `KRITIK_CONNECTIONS_ACCOUNTS`                  | `accounts`, comma-separated           |
| `KRITIK_CONNECTIONS_APP_CLIENT_ID`             | `app.clientId`                        |
| `KRITIK_CONNECTIONS_APP_PRIVATE_KEY[_FILE]`    | `app.privateKey`, or a file's path    |
| `KRITIK_CONNECTIONS_APP_WEBHOOK_SECRET[_FILE]` | `app.webhookSecret`, or a file's path |

The environment declares at most one connection. It replaces the file's
connection of the same name whole, or is added to the file's when none
has that name. A `KRITIK_CONNECTIONS_*` variable that names no key is
refused at startup.

In the admin console, the instance configuration's "Add connection" takes
the same values, seals the secrets in Postgres, and can generate the
webhook secret for you to copy into the App. A dashboard connection may
not take a name or an account that a file connection declares. When a
later file edit declares one that a dashboard connection already holds,
the file's connection is left out, and the admin console says why.

### Install the App

Install the App on each account in `accounts`, for all repositories or
selected ones. Each pull request in them is reviewed when it opens and
after each push, under the account's settings, which an admin sets on the
account's admin page.

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
