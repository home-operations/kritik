# ADR-0012: a dashboard GitHub installation can create its App from a manifest

- **Status:** Superseded
- **Date:** 2026-09-26
- **Superseded by:** [ADR-0019](0019-configuration-in-git.md): the
  dashboard no longer registers an App; it is created on GitHub by hand.
- **Amended by:** [ADR-0014](0014-github-app-only-self-hosted.md), which
  runs the flow for the instance rather than a tenant, lets the admin
  choose a private or public App, shows the `client_secret` once for the
  App to double as the GitHub sign-in, and drops `KRITIK_HOOKS_URL` of
  §2.3 for `/hooks` under the dashboard's URL.
- **Authors:** onedr0p.

> Scope: how a dashboard-managed tenant's GitHub installation gets its
> GitHub App: registered by kritik from a manifest, with its webhook,
> permissions and events already set, instead of by hand. It does not
> change how webhooks are routed or verified, file-managed installations,
> or Forgejo.

## 1. Context

[Connecting a forge](../connecting-a-forge.md) makes a GitHub App's own
webhook the way GitHub events reach kritik: one webhook per App, pointed at
`/hooks/<installation>`, covering every repository the App is installed on.
Registering that App by hand takes a webhook URL and secret, five
repository permissions and four events, then copying a client ID, a private
key and the secret into kritik. The mistakes fail quietly. An App without
the Issues permission cannot subscribe to `issue_comment`, the one event
that carries a pull request's conversation comments
([webhook events](https://docs.github.com/en/webhooks/webhook-events-and-payloads)),
so its mentions go unanswered. An App with no webhook leaves kritik polling,
which the tenant overview's Installations panel now points out but cannot
fix.

GitHub registers an App from a JSON manifest
([registering from a manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)):

1. A form POSTs the manifest, in a `manifest` field, to
   `https://github.com/settings/apps/new` for a personal account or
   `https://github.com/organizations/<org>/settings/apps/new` for an
   organization, with an unguessable `state` against cross-site request
   forgery. The person confirms there, and may rename the App.
2. GitHub redirects to the manifest's `redirect_url` with `code` and
   `state`.
3. `POST /app-manifests/{code}/conversions` returns the App's `id`, `slug`,
   `client_id`, `client_secret`, `webhook_secret` and `pem`
   ([REST reference](https://docs.github.com/en/rest/apps/apps#create-a-github-app-from-a-manifest)).
   GitHub creates the webhook secret itself.

All three steps must finish within an hour. The manifest sets `name`, `url`
(required), `hook_attributes` (`url` required, `active`), `redirect_url`,
`setup_url`, `public`, `default_permissions` and `default_events`, among
others. The conversion endpoint documents no authentication and does not
work with user or installation tokens; it is called without credentials,
as Probot does.

kritik already has most of what the flow needs:

- dashboard-managed installations whose secrets are sealed at rest and
  bound to the installation's forge, host and account;
- a signed-in admin session to bind the flow to;
- the dashboard's origin, `KRITIK_WEB_URL`, for the redirect.

It does not know the webhook listener's public origin, which may be served
from another host than the dashboard.

## 2. Decision

### 2.1 The installation form offers "Create GitHub App"

For a `github` installation of a dashboard-managed tenant on github.com, an
admin can create the App instead of entering its client ID, private key and
webhook secret. The form asks whether the account is a personal account or
an organization, since the two register Apps at different URLs, and submits
this manifest to GitHub:

```json
{
  "name": "kritik-<tenant slug>",
  "url": "<KRITIK_WEB_URL>",
  "hook_attributes": { "url": "<KRITIK_HOOKS_URL>/hooks/<installation>", "active": true },
  "redirect_url": "<KRITIK_WEB_URL>/<callback path>",
  "setup_url": "<KRITIK_WEB_URL>/<the tenant's page>",
  "public": false,
  "default_permissions": {
    "contents": "read",
    "metadata": "read",
    "pull_requests": "write",
    "issues": "read",
    "statuses": "write"
  },
  "default_events": ["pull_request", "pull_request_review_comment", "issue_comment", "push"]
}
```

The permissions and events are the guide's, kept in one place in the code
so the two cannot drift. `public: false` keeps the App installable only on
the account that owns it, the only account the installation serves.

### 2.2 The callback stores the credentials like an admin's secrets

- `state` is random and single-use, and is kept server-side with the
  admin's session, the tenant, the installation name and the account. It
  expires after an hour, as the code does. A callback without a matching
  state is refused.
- The callback converts the code and stores `client_id`, `pem` and
  `webhook_secret` as the installation's `app.clientId`, `app.privateKey`
  and `app.webhookSecret`: sealed and bound as secrets an admin submits
  are. It writes the tenant's next revision and an audit event, as saving
  the form does. `client_secret` is not kept: kritik never signs anyone in
  through this App.
- The page then links `https://github.com/apps/<slug>/installations/new`
  to install the App on the account. `setup_url` brings the admin back to
  the tenant afterwards; kritik resolves installations per repository and
  takes nothing from the installation id `setup_url` receives.

### 2.3 The listener's public origin is a setting

`KRITIK_HOOKS_URL` is the origin the forge reaches `/hooks` at, configured
like `KRITIK_WEB_URL`. The form offers the action only when it is set;
otherwise the guide's manual steps apply.

## 3. Consequences

- A dashboard GitHub installation gets an App with the right webhook,
  secret, permissions and events in one confirmation on GitHub. The admin
  never handles the private key.
- kritik gains a callback route, a place to keep `state`, and
  `KRITIK_HOOKS_URL`. The callback does nothing without a state issued to
  the same session.
- The permissions and events are fixed when the App is created. When kritik
  later needs another permission, an existing App gets it only once its
  owner edits the App on GitHub.
- Each tenant still owns its App. Nothing changes for file-managed
  installations or Forgejo.

## 4. Rejected alternatives

- **One public App for the whole instance**, which tenants only install:
  the hosted-service model, and the "instance's shared one" ADR-0002
  allows. Every tenant's deliveries then share one webhook URL, so they
  have to be routed by the delivery's account rather than by the hook
  path, and the operator owns an App every tenant depends on. It suits a
  hosted instance, not a self-hosted one where each tenant owns its App. It
  would be a separate decision, and this one does not rule it out.
- **Registration stays manual**, with the guide and the Installations
  panel. That remains the path when `KRITIK_HOOKS_URL` is unset, and for
  file-managed installations.

## 5. Deferred

- **File-managed installations.** Their secrets live in the operator's
  Secrets, which kritik does not write. The operator console could run the
  same flow and show the credentials once, as it shows generated webhook
  secrets.
- **GitHub Enterprise Server.** Whether the flow works on an Enterprise
  host as kritik's GitHub Enterprise installations reach it is not yet
  checked.
- **Looking up the account type** (`GET /users/{account}`) instead of
  asking.
