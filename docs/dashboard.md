# Dashboard

The web role serves a dashboard: sign in with GitHub or any OIDC provider,
and see the tenants you belong to, their installations and
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
  the callback URL and in `operators`), a `type` of `oidc` or `github`, a
  `clientId`, and a `clientSecret` (a secret
  reference: `env`, `file` or `sealed`). Every provider must allow the
  callback URL
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
  ```

  `oidc` takes `issuer` (an `https` URL); `github` signs in on github.com
  and takes no `issuer`.

- `operators` — the identities allowed to change configuration, each
  `"<signIn name>:<login or subject>"` (the forge login or OIDC subject) or
  `"email:<address>"`, matched against the address a sign-in reports.
  `email` is reserved and cannot name a `signIn`. An operator creates and
  deletes dashboard tenants from the operator console and is the only one
  who may set the operator-only fields below. The console also lists the
  instance settings read-only, each with its source: the web process's
  environment, the configuration file, or kritik's default. A secret shows
  only whether it is set, and a URL's credentials are hidden.
- `sessionTTL` — how long a dashboard session lasts, between 5 minutes and
  30 days; defaults to 12 hours.
- `dashboardProviderHosts` — the hosts a dashboard-managed tenant's own
  provider keys may name in `baseUrl`. The worker calls a provider from
  inside the cluster with its key, so this bounds where a tenant admin can
  aim it; empty allows only each provider type's own endpoint (no
  `baseUrl`). A `baseUrl` must be `https` on port 443. No wildcards.

## Roles

Three roles share the same `web.signIn` and `web.operators`:

- **Operator** — an identity in `web.operators`. The only one who can edit
  the configuration file, the only way a dashboard tenant is created, and
  the only one who may set a dashboard tenant's `runner` and `limits`, or
  its `models` (on the operator's providers), `forks`, `mode`, `agent`,
  `incremental` or `allow` at the tenant or on any of its
  `repositories[]`; a tenant admin's write that touches any of those is
  rejected. The rule is the policy table in
  `internal/configfile/policy.go`: the tenant configuration read serves it
  with what the caller may change there, and the form disables the rest. Membership is checked per source (the forge, refreshed
  at sign-in, and accepted invites), and a principal who qualifies through
  more than one gets the highest of the roles it grants.
- **Tenant admin** — can edit a dashboard-managed tenant's configuration,
  installations, repositories and provider keys (other than the
  operator-only fields above), set its review and fallback models to one
  on its own provider keys, invite and remove members, and queue a re-run, cancel or
  reindex; every one of those writes is audit-logged in the same
  transaction as the change it makes. A secret an admin submits (an App's
  private key or client ID) is bound to that installation's forge and
  accounts: change either and the secret must be re-entered, since it no
  longer acts for the same accounts. The form never keeps a renamed
  installation's secrets. In the advanced JSON editor, as through the API,
  `{"keep": true}` keeps the secret stored under the name the JSON gives:
  renaming an installation there does not carry its secrets along (the
  keep is refused, or takes the secret of a stored installation that
  already had the new name, when its forge and accounts match), so
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

## Provider keys

A tenant can bring its own model keys: `providers` in its spec, the same
shape as the file's top-level `providers`, edited in the dashboard's
"Provider keys" section. A model named `<key name>/<model>` then runs on
that key, and the tenant pays for it; a key's name may not be one the
file's providers already use. A tenant admin may set the tenant's review
and fallback models, and a repository entry's, to a model on one of these
keys or clear them; a model on the operator's providers stays the
operator's to set. The keys are sealed at rest like installation secrets
and never shown again. A saved key is kept only while its name, type and
endpoint stay the same, so a key cannot be sent anywhere it was not
entered for. Tenant limits still apply to runs on a tenant's own key.

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
- A file tenant whose slug or installation name a dashboard tenant already
  holds is left out of the running configuration, at boot or on reload,
  while every other tenant runs: the operator console lists it with the
  reason and `kritik_config_error{stage="merge"}` stays at 1. Rename either
  side, or delete the dashboard tenant, to bring it back.
- A secret referenced by `file:` is only re-read when the configuration
  file itself changes, not on the referenced file's own schedule: rotate
  the file, then touch or reapply the configuration to pick it up.
- An `email:` operator, or an email invite, is only as trustworthy as the
  forge or IdP's own email verification — kritik does not verify addresses
  itself, it trusts what the sign-in reports.
- The web role only ever holds the application database DSN, never the
  owner DSN a migration or leader election needs, and refuses to start if
  it would.
