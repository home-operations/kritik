# Dashboard

The web role serves a dashboard: sign in with GitHub, Forgejo or any OIDC
provider, and see the tenants you belong to, their installations and
repositories, live review and conversation state as it runs, member and
invite management, and a per-tenant and (for operators) instance-wide audit
log. A tenant admin can also queue a re-run of a specific pull request,
cancel a review in progress, or reindex a repository's embeddings, from the
dashboard rather than the forge.

Enable it with role `web` (a dedicated listener) or `all` (which also serves
it once `KRITIK_WEB_URL` is set, alongside the other roles); `KRITIK_WEB_ADDR`
is where it listens (default `:8083`), and `KRITIK_WEB_URL` is its
externally reachable origin — an absolute `http(s)` URL with no query or
fragment, required for the web role, used to build sign-in callback URLs and
the session cookie's scope. In the chart, `roles.web.enabled` turns the role
on, `web.url` sets `KRITIK_WEB_URL`, and `web.port` matches `KRITIK_WEB_ADDR`'s
port (8083 by default); `ingress.web` and `httpRoute.web` are the Ingress and
Gateway API HTTPRoute for it, and `dashboard.keySecret` names the Secret
holding the sealing key (below).

## `web:` configuration reference

The `web:` block, a sibling of `tenants:` at the file's root, controls who
may sign in and who of them may operate the instance:

- `signIn` — one entry per identity provider, each with a `name` (used in
  the callback URL and in `operators`), a `type` of `oidc`, `github` or
  `forgejo`, a `clientId`, and a `clientSecret` (a secret reference: `env`,
  `file` or `sealed`). Every provider must allow the callback URL
  `<KRITIK_WEB_URL>/auth/callback/<name>`. For example:

  ```yaml
  web:
    signIn:
      - name: sso
        type: oidc
        issuer: https://idp.example.com
        clientId: kritik-dashboard
        clientSecret: { env: OIDC_CLIENT_SECRET }
        scopes: [openid, email, profile]
      - name: github
        type: github
        clientId: Iv1.abc123
        clientSecret: { env: GITHUB_CLIENT_SECRET }
      - name: ghe
        type: github
        host: github.example.com
        clientId: abc123
        clientSecret: { file: /run/secrets/ghe-client-secret }
      - name: forgejo
        type: forgejo
        host: forgejo.example.com
        clientId: abc123
        clientSecret: { env: FORGEJO_CLIENT_SECRET }
  ```

  `oidc` takes `issuer` (an `https` URL) and no `host`; `github` and
  `forgejo` take `host` and no `issuer`. A `github` sign-in with no `host`
  is `github.com` — a GitHub Enterprise instance is still `type: github`,
  just naming its own `host`; `forgejo`'s `host` is always required.

- `operators` — the identities allowed to change configuration, each
  `"<signIn name>:<login or subject>"` (the forge login or OIDC subject) or
  `"email:<address>"`, matched against the address a sign-in reports.
  `email` is reserved and cannot name a `signIn`. An operator creates and
  deletes dashboard tenants from the operator console and is the only one
  who may set the operator-only fields below.
- `sessionTTL` — how long a dashboard session lasts, between 5 minutes and
  30 days; defaults to 12 hours.
- `dashboardForgeHosts` — the forge hosts a dashboard-managed tenant's
  installations may use. Every installation host is also an allowed runner
  egress host, so this bounds what a tenant admin, who did not write the
  operator's file, can point kritik at; empty means `github.com` plus
  whatever hosts the file's own installations already use. No wildcards.

## Roles

Three roles share the same `web.signIn` and `web.operators`:

- **Operator** — an identity in `web.operators`. The only one who can edit
  the configuration file, the only way a dashboard tenant is created, and
  the only one who may set a dashboard tenant's `models`, `forks`,
  `runner`, `limits`, `repositories[].agent`, `repositories[].mode` or
  `repositories[].incremental`; a tenant admin's write that touches any of
  those is rejected. Membership is checked per source (the forge, refreshed
  at sign-in, and accepted invites), and a principal who qualifies through
  more than one gets the highest of the roles it grants.
- **Tenant admin** — can edit a dashboard-managed tenant's configuration,
  installations and repositories (other than the operator-only fields
  above), invite and remove members, and queue a re-run, cancel or
  reindex; every one of those writes is audit-logged in the same
  transaction as the change it makes. A dashboard installation may only
  reach its forge over `https`; a plain-`http` host is refused. A secret an
  admin submits (a client secret, an installation token) is bound to that
  installation's forge, host (scheme and path included) and account —
  change any of them and the secret must be re-entered, since it no longer
  speaks for the same identity. The form never keeps a renamed
  installation's secrets. In the advanced JSON editor, as through the API,
  `{"keep": true}` keeps the secret stored under the name the JSON gives:
  renaming an installation there does not carry its secrets along (the
  keep is refused, or takes the secret of a stored installation that
  already had the new name, when its forge, host and account match), so
  enter them again when renaming in JSON.
- **Tenant member** — read access to their tenant's own reviews,
  conversations and transcripts; no write access.

Re-run, cancel and reindex all respond `202 Accepted` (with a job ID for
re-run and reindex) and queue the work rather than running it inline;
re-running a pull request with no known head, or cancelling a review that
is not running, is a `409 Conflict`. Inviting an address that is already a
member of the tenant is refused, `409 already_member`, instead of creating
a duplicate, and claiming a slug another tenant already holds, file- or
dashboard-managed, is `409 slug_taken`. Deleting a dashboard tenant removes
its memberships and invites along with it, but not its history: its
reviews, findings, usage and transcripts stay keyed on its slug. Creating a
tenant under a slug any tenant held before is therefore also `409
slug_taken`, unless the operator creates it with `adopt` (offered in the
operator console after that refusal): the new tenant starts with none of
the old one's members or invites but keeps its review history, visible to
the new tenant's members. An installation name stays with the tenant that
first held it, even once that tenant is gone.

## Sealing key

A dashboard-managed tenant's secrets are sealed at rest with an instance
key, `KRITIK_DASHBOARD_KEY` / `dashboard.keySecret`: generate one with
`openssl rand -base64 32`. To rotate it, move the old value into
`KRITIK_DASHBOARD_OLD_KEYS` / `dashboard.oldKeysSecret` (comma-separated,
accepted only to open values already sealed under it), and set a freshly
generated value as `KRITIK_DASHBOARD_KEY`. A value sealed under an old key
is re-sealed under the current one the next time it is written, not
eagerly on rotation, so keep an old key listed until every value under it
has been touched at least once.

## `retention.transcripts`

A top-level `retention.transcripts` (default 30 days, minimum 24 hours)
controls how long an agentic review's full model transcript is kept; the
review itself, its findings and its comments outlive it. A transcript may
contain repository content the agent read while investigating, and it is
visible to every member of the tenant it belongs to, not only admins.

## Operational notes

- A dashboard tenant that fails to merge into the configuration at boot
  fails startup the same as a bad configuration file: fix the offending
  row or the file. A merge or apply failure after boot instead keeps the
  last good configuration running and raises the `kritik_config_error`
  gauge (labelled `merge` or `apply`) until a later attempt succeeds.
- A secret referenced by `file:` is only re-read when the configuration
  file itself changes, not on the referenced file's own schedule: rotate
  the file, then touch or reapply the configuration to pick it up.
- An `email:` operator, or an email invite, is only as trustworthy as the
  forge or IdP's own email verification — kritik does not verify addresses
  itself, it trusts what the sign-in reports.
- The web role only ever holds the application database DSN, never the
  owner DSN a migration or leader election needs, and refuses to start if
  it would.
