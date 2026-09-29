# Configuration

kritik takes its settings from three places, each for what it suits:

| Where                                                                                | What                                                                                                             | Changed by                                         |
| ------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| The environment                                                                      | Process wiring: addresses, the database, logging, the sealing key and `KRITIK_WEB_URL`                           | a restart                                          |
| The configuration file, and its `KRITIK_AUTH_*` and `KRITIK_CONNECTIONS_*` variables | How people sign in (`auth`), and GitHub Apps fixed at deploy time (`connections`)                                | a file edit, reloaded, or a restart for a variable |
| The instance configuration                                                           | Everything else: model providers, the embedder, defaults, accounts, repositories and the dashboard's connections | the [dashboard](dashboard.md)                      |

The file is optional: `KRITIK_CONFIG_FILE` names it, and the chart's
`config.file` renders it. It holds `auth` and `connections`, and nothing
else. Every key in it also has a variable, and a variable wins over the
file, so a deployment can be configured from the environment alone. In
the file a secret is `{ env: NAME }` or `{ file: path }`; its variable
carries the value itself, or, with a `_FILE` suffix, the path of a file
holding it. A `KRITIK_AUTH_*` or `KRITIK_CONNECTIONS_*` variable that names
no key is refused at startup rather than ignored.

## `auth`

`auth` sets how people sign in and what each may do. The chart's `auth`
values render its variables.

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
  App's own. An App the admin console creates can serve both: it shows the
  App's client secret once for this block (see
  [setup](setup.md)).
- `sessionTTL` is how long a dashboard session lasts, between 5 minutes and
  30 days. It defaults to 12 hours.

A provider must allow the callback URL `<KRITIK_WEB_URL>/auth/callback/oidc`
or `<KRITIK_WEB_URL>/auth/callback/github`. The dashboard refuses to start
with no way to sign in, and a running one keeps its last good configuration
when a reload would leave none. The configuration is refused when nothing
could make an admin: set an admin password, or a `roleMapping` on a
provider.

### Roles

There are two roles:

- **Admin** manages the instance. An admin edits the
  [instance configuration](dashboard.md#instance-configuration), queues
  re-runs, cancels and reindexes, and reads every account and the audit
  log. The admin console also lists the instance settings
  read-only, each with its source: the environment, the configuration
  file, or kritik's default. A secret shows only whether it is set, and a
  URL's credentials are hidden. Every write is audit-logged in the same
  transaction as the change it makes.
- **Member** reads reviews, conversations and transcripts, with no write
  access. A member reads every account, or only the accounts serving the
  forge accounts their sign-in placed them on.

A session holds the role its sign-in gave it. Editing a provider's role
mapping, or rotating the admin password, ends the sessions it granted, so
the next request signs in again under the new rules.

#### Role mappings

A `roleMapping` is a [CEL](https://cel.dev) expression evaluated at
sign-in. It yields a role for every account, `"admin"`, `"member"` or `""`
for none. Or it yields a map from forge account to `"member"`, which reads
only the accounts serving those accounts, such as
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
  every account. `defaultRole` defaults to `none`, so a wrong mapping fails
  closed.
- A GitHub sign-in reads the accounts serving the user's own account, or an
  organization they are an active member of, and is refused when there are
  none. Accounts a mapping names are added to those.

A mapping that fails to evaluate refuses the sign-in.

## `connections`

`connections` declares GitHub Apps fixed at deploy time, which the
dashboard shows but does not change. The [setup guide](setup.md) covers
creating the App.

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
