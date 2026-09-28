# Dashboard

The web role serves a dashboard: sign in with a local admin password,
GitHub or an OIDC provider, and see the tenants you can read, their
connections and repositories, live review and conversation state as it
runs, and, for an admin, the audit log. An admin can also queue a re-run
of a specific pull request, cancel a review in progress, or reindex a
repository's embeddings, from the dashboard rather than the forge.

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

## Signing in

The `auth:` block, a sibling of `tenants:` at the file's root, sets how
people sign in and what each may do. Every key in it also has a
`KRITIK_AUTH_*` environment variable, and a variable wins over the file, so
a deployment can configure sign-in from the environment alone. A secret's
variable carries the value itself, or, with a `_FILE` suffix, the path of a
file holding it. A `KRITIK_AUTH_*` variable that names no key is refused at
startup rather than ignored.

| Key                   | Environment variable                       |
| --------------------- | ------------------------------------------ |
| `sessionTTL`          | `KRITIK_AUTH_SESSION_TTL`                  |
| `admin.user`          | `KRITIK_AUTH_ADMIN_USER`                   |
| `admin.password`      | `KRITIK_AUTH_ADMIN_PASSWORD[_FILE]`        |
| `oidc.name`           | `KRITIK_AUTH_OIDC_NAME`                    |
| `oidc.issuer`         | `KRITIK_AUTH_OIDC_ISSUER`                  |
| `oidc.clientId`       | `KRITIK_AUTH_OIDC_CLIENT_ID`               |
| `oidc.clientSecret`   | `KRITIK_AUTH_OIDC_CLIENT_SECRET[_FILE]`    |
| `oidc.scopes`         | `KRITIK_AUTH_OIDC_SCOPES`, comma-separated |
| `oidc.rolesClaim`     | `KRITIK_AUTH_OIDC_ROLES_CLAIM`             |
| `oidc.roleMapping`    | `KRITIK_AUTH_OIDC_ROLE_MAPPING`            |
| `oidc.defaultRole`    | `KRITIK_AUTH_OIDC_DEFAULT_ROLE`            |
| `github.clientId`     | `KRITIK_AUTH_GITHUB_CLIENT_ID`             |
| `github.clientSecret` | `KRITIK_AUTH_GITHUB_CLIENT_SECRET[_FILE]`  |
| `github.roleMapping`  | `KRITIK_AUTH_GITHUB_ROLE_MAPPING`          |

```yaml
auth:
  admin:
    password: { env: ADMIN_PASSWORD }
  oidc:
    name: Company SSO
    issuer: https://idp.example.com
    clientId: kritik-dashboard
    clientSecret: { env: OIDC_CLIENT_SECRET }
    scopes: [openid, email, profile]
    rolesClaim: groups
    roleMapping: '"kritik-admins" in roles ? "admin" : ("kritik-users" in roles ? "member" : "")'
  github:
    clientId: Iv1.abc123
    clientSecret: { env: GITHUB_CLIENT_SECRET }
    roleMapping: 'login == "user-1" ? "admin" : ""'
```

- `admin` is the local admin. It signs in on the sign-in page with a
  username, `admin` unless `user` sets another, and `password`. It exists
  only while a password is set: it is the way into a fresh instance, and a
  way in when every provider is down. Ten failed attempts from one address
  within 15 minutes lock that address out until the window passes.
- `oidc` signs in through any OpenID Connect issuer, an `https` URL. The
  sign-in page labels it `name`, or "SSO" when unset.
- `github` signs in on github.com with an OAuth App's client, or a GitHub
  App's own.
- `sessionTTL` is how long a dashboard session lasts, between 5 minutes and
  30 days. It defaults to 12 hours.

A provider must allow the callback URL `<KRITIK_WEB_URL>/auth/callback/oidc`
or `<KRITIK_WEB_URL>/auth/callback/github`. The dashboard refuses to start
with no way to sign in. The configuration is refused when nothing could
make an admin: set an admin password, or a `roleMapping` on a provider.

## Roles

There are two roles:

- **Admin** manages the instance. An admin creates, edits and deletes
  dashboard tenants with their connections, repositories and provider
  keys, queues re-runs, cancels and reindexes, and reads every tenant and
  the audit log. The admin console also lists the instance settings
  read-only, each with its source: the environment, the configuration
  file, or kritik's default. A secret shows only whether it is set, and a
  URL's credentials are hidden. Every write is audit-logged in the same
  transaction as the change it makes.
- **Member** reads reviews, conversations and transcripts, with no write
  access. A member reads every tenant, or only the tenants serving the
  forge accounts their sign-in placed them on.

A session holds the role its sign-in gave it. Editing a provider's role
mapping, or rotating the admin password, ends the sessions it granted, so
the next request signs in again under the new rules.

### Role mappings

A `roleMapping` is a [CEL](https://cel.dev) expression evaluated at
sign-in. It yields a role for every tenant, `"admin"`, `"member"` or `""`
for none. Or it yields a map from forge account to `"member"`, which reads
only the tenants serving those accounts, such as
`{"github/org-1": "member"}`; `"*"` as a key stands for every account. CEL
gives both branches of a conditional one type, so an expression that
yields a role on one branch and a map on the other wraps one in `dyn()`.

An OIDC mapping sees `claims`, the ID token's claims merged with the
UserInfo response, and `roles`, the values of the claim `rolesClaim`
names, read from a list, a map's keys, or a single string. A GitHub
mapping sees `login`, `email`, `orgs`, the organizations the user is an
active member of, and `teams`, each as `"<org>/<team>"`.

When the mapping places nobody:

- An OIDC sign-in is refused, unless `defaultRole: member` lets it read
  every tenant. `defaultRole` defaults to `none`, so a wrong mapping fails
  closed.
- A GitHub sign-in reads the tenants serving the user's own account, or an
  organization they are an active member of, and is refused when there are
  none. Accounts a mapping names are added to those.

A mapping that fails to evaluate refuses the sign-in.

## Tenant writes

A secret an admin submits, such as an App's private key or client ID, is
bound to that connection's forge and accounts: change either and the
secret must be re-entered, since it no longer acts for the same accounts.
The form never keeps a renamed connection's secrets. In the advanced
JSON editor, as through the API, `{"keep": true}` keeps the secret stored
under the name the JSON gives: renaming a connection there does not
carry its secrets along. The keep is refused, or takes the secret of a
stored connection that already had the new name when its forge and
accounts match, so enter them again when renaming in JSON.

Re-run, cancel and reindex all respond `202 Accepted`, with a job ID for
re-run and reindex, and queue the work rather than running it inline.
Re-running a pull request with no known head, or cancelling a review that
is not running, is a `409 Conflict`. Claiming a slug another tenant already
holds, file- or dashboard-managed, is `409 slug_taken`. Deleting a
dashboard tenant does not delete its history: its reviews, findings, usage
and transcripts stay keyed on its slug. Creating a tenant under a slug any
tenant held before is therefore also `409 slug_taken`, unless the admin
creates it with `adopt`, offered in the admin console after that refusal.
The new tenant keeps the old one's review history. A connection name
stays with the tenant that first held it, even once that tenant is gone.

## Provider keys

A tenant can bring its own model keys: `providers` in its spec, the same
shape as the file's top-level `providers`, edited in the dashboard's
"Provider keys" section. A model named `<key name>/<model>` then runs on
that key, and the tenant pays for it; a key's name may not be one the
file's providers already use. The tenant's review and fallback models,
and a repository entry's, may name a model on one of these keys. The keys are sealed at rest like connection secrets
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
- A file tenant whose slug or connection name a dashboard tenant already
  holds is left out of the running configuration, at boot or on reload,
  while every other tenant runs: the admin console lists it with the
  reason and `kritik_config_error{stage="merge"}` stays at 1. Rename either
  side, or delete the dashboard tenant, to bring it back.
- A secret referenced by `file:` is only re-read when the configuration
  file itself changes, not on the referenced file's own schedule: rotate
  the file, then touch or reapply the configuration to pick it up.
- A role mapping is only as trustworthy as what it reads. Map on groups
  or roles the IdP controls, not on an email or name a user can set on
  their own profile.
- The web role only ever holds the application database DSN, never the
  owner DSN a migration or leader election needs, and refuses to start if
  it would.
