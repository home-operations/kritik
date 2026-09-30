# Configuration

kritik takes its settings from three places, each for what it suits
([ADR-0019](adr/0019-configuration-in-git.md)):

| Where                                                                                                                                                 | What                                                                                                                                                                        | Changed by                                         |
| ----------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| The environment                                                                                                                                       | Process wiring: addresses, the database, logging and `KRITIK_WEB_URL`                                                                                                       | a restart                                          |
| The configuration file, and its `KRITIK_AUTH_*`, `KRITIK_CONNECTIONS_*`, `KRITIK_PROVIDERS_*`, `KRITIK_DEFAULTS_*` and `KRITIK_EMBEDDING_*` variables | Everything else: sign-in, the GitHub Apps, model providers, the defaults, the embedder, the accounts and their repositories, polling, indexing, tools, retention and egress | a file edit, reloaded, or a restart for a variable |
| The dashboard                                                                                                                                         | Whether each repository is on or off                                                                                                                                        | an admin, on the Repositories page                 |

A repository's own [`.kritik.yaml`](repository-config.md) narrows what the
configuration sets for it, from its own git.

The file is optional: `KRITIK_CONFIG_FILE` names it, and the chart's
`config.file` renders it, so the configuration lives in git with the rest
of the deployment. In the file a secret is `{ env: NAME }` or
`{ file: path }`, never the value itself. Sign-in, one connection, one
provider, the default models and review settings, and the embedder also
have variables, and a variable wins over the file, so a small deployment
can be configured from the environment alone. A variable carries a secret
itself, or, with a `_FILE` suffix, the path of a file holding it. A
variable under one of these prefixes that names no key is refused at
startup rather than ignored.

Every replica re-reads the file on the chart's `config.reloadInterval`.
Content that does not load, or that would leave the dashboard no way to
sign in, is refused: the configuration before it keeps running,
`kritik_config_error{stage="load"}` is 1, and the Configuration page, under
Settings, says why until the file loads again. At startup the same content
fails the process instead.

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
  App's own: the App kritik reviews through can serve both, with a client
  secret generated on its settings page (see [setup](setup.md)).
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

- **Admin** runs the instance from the dashboard. An admin turns
  repositories on and off, queues re-runs, cancels and reindexes, and reads
  every account, the audit log and the
  [Configuration page](dashboard.md#configuration-page), which lists the
  instance settings read-only, each with its source: the environment, the
  configuration file, or kritik's default. A secret shows only whether it
  is set, and a URL's credentials are hidden. Every write is audit-logged
  in the same transaction as the change it makes.
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

`connections` declares the GitHub Apps kritik serves accounts through. The
[setup guide](setup.md) covers creating the App.

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

## Instance defaults: `providers`, `defaults` and `embedding`

`providers` are the instance's model keys, `defaults` the settings every
account and repository inherits unless its entry sets its own, and
`embedding` the embedder that builds each repository's similar-code index.

```yaml
providers:
  openrouter:
    type: openrouter
    apiKey: { file: /var/run/secrets/kritik/openrouter/api-key }
defaults:
  models:
    review: openrouter/vendor/large-model
    fallback: openrouter/vendor/small-model
  mode: agentic
  review: { feedback: detailed }
  forks: false
  settle: 30s
embedding:
  baseUrl: https://openrouter.ai/api/v1
  apiKey: { file: /var/run/secrets/kritik/openrouter/api-key }
  model: vendor/embedding-model
  dims: 1024
```

A provider is `type` (`openrouter`, `openai` or `anthropic`), an optional
`baseUrl` and `pricing`, and its `apiKey`. A model is named
`<provider>/<model>`, on a provider the file declares.
`mode` is `agentic` (the default, which needs the chart's `gateway`) or
`single`, `review.feedback` is `detailed`, `standard` or `minimal`
([repository settings](repository-config.md)), `forks: true`
reviews pull requests from forks without being asked (by default one is
reviewed only when a maintainer comments `@<app slug> review`), and
`settle` delays a review after a push so a burst of pushes is reviewed
once. `defaults` also takes the rest of an account's settings (below), as
what every account inherits.

The same defaults can come from the environment:

| Variable                          | Key                                                                                   |
| --------------------------------- | ------------------------------------------------------------------------------------- |
| `KRITIK_PROVIDERS_NAME`           | the provider's name, `openrouter` unless set                                          |
| `KRITIK_PROVIDERS_TYPE`           | `type`, which defaults to the name when that is `openrouter`, `openai` or `anthropic` |
| `KRITIK_PROVIDERS_BASE_URL`       | `baseUrl`                                                                             |
| `KRITIK_PROVIDERS_API_KEY[_FILE]` | `apiKey`, or a file's path                                                            |
| `KRITIK_DEFAULTS_MODELS_REVIEW`   | `defaults.models.review`                                                              |
| `KRITIK_DEFAULTS_MODELS_FALLBACK` | `defaults.models.fallback`                                                            |
| `KRITIK_DEFAULTS_MODE`            | `defaults.mode`                                                                       |
| `KRITIK_DEFAULTS_FEEDBACK`        | `defaults.review.feedback`                                                            |
| `KRITIK_DEFAULTS_FORKS`           | `defaults.forks`, `true` or `false`                                                   |
| `KRITIK_DEFAULTS_SETTLE`          | `defaults.settle`, a duration such as `30s`                                           |
| `KRITIK_EMBEDDING_BASE_URL`       | `embedding.baseUrl`                                                                   |
| `KRITIK_EMBEDDING_API_KEY[_FILE]` | `embedding.apiKey`, or a file's path                                                  |
| `KRITIK_EMBEDDING_MODEL`          | `embedding.model`                                                                     |
| `KRITIK_EMBEDDING_DIMS`           | `embedding.dims`                                                                      |

The environment declares at most one provider, which replaces the file's
of the same name whole or is added to the file's. The embedding variables
set the file's embedder key by key. The Configuration page lists what the
file and the environment set under "Instance settings", with where each
comes from.

The embedder is any OpenAI-compatible embeddings endpoint: `baseUrl`,
`apiKey`, `model` and `dims`, at most 4000, and the optional `maxBatch`,
`maxBatchChars` and `maxItemChars`, which bound one request to 64 inputs,
200,000 characters and 16,000 characters per input unless set. Without an
embedder, indexing is off and reviews run without similar code. The index
holds one model and dimension: a configuration that changes either drops
every repository's index, and the leader builds each again, a few at a
time, as `indexing.onboardWindow` paces them. Removing the embedder keeps
the index, and adding back the same model and dimension uses it again.

## `accounts`

An account is a user or organization a connection serves, `github/<name>`.
It runs with the defaults unless `accounts` has an entry for it, keyed by
forge and name, which sets its own settings, and each repository entry
under it names a repository without its owner:

```yaml
accounts:
  - forge: github
    name: org-1
    models: { review: openrouter/vendor/large-model }
    limits: { reviewsPerDay: 50, tokensPerMonth: 20000000 }
    review:
      rules:
        - id: wrap-errors
          rule: 'Wrap an error with fmt.Errorf("<package>: %w", err) before returning it.'
          paths: ["**/*.go"]
    repositories:
      - name: repo-1
        mode: single
        settle: 2m
```

An account and a repository entry take the defaults' settings: `models`,
`mode`, `filter`, `forks`, `ignore`, `settle`, `agent`, `incremental`,
`review` (rules, templates, context and the rest of
[the repository settings](repository-config.md)) and `allow`, the bounds a
repository's `.kritik.yaml` chooses within. A narrower scope's value
replaces the broader one's, except `ignore` globs, which add up, and
`review.rules`, which add up by id ([ADR-0018](adr/0018-rules.md)). An
account also takes `limits` (`concurrency`, `reviewsPerDay`,
`tokensPerMonth`), `runner` (the review and index Jobs' `resources` and
`activeDeadlineSeconds`), and `providers`, its own model keys: a model
named `<key name>/<model>` then runs on that key and the account pays for
it, and a key's name may not be one the instance's providers use.

An account runs while a connection serves it. An entry for an account no
connection serves is kept, but not run, and the Configuration page lists
it as not served.

### Which repositories run

kritik registers every repository each connection's App reaches, once the
configuration is applied and again on every poll, and "Resync from GitHub"
on the Repositories page does the same at once. Whether one runs is
decided in this order ([ADR-0019](adr/0019-configuration-in-git.md)):

1. an archived repository never runs: unarchive it on GitHub, then
   resync;
2. one an admin turned on or off on the Repositories page runs as they
   chose;
3. a fork does not run, since an account can reach many forks it never
   meant to review;
4. any other runs as `enabled` says, at the defaults or the account: on
   unless one sets `enabled: false`.

A repository entry may not set `enabled`: the dashboard owns a
repository's on or off, and the configuration only says where one starts.
A repository that is off is neither reviewed, polled nor indexed. One an
admin turned off, or that the App no longer reaches, has its index dropped
once `retention.disabledIndexGrace` has passed.

## Other settings

- `polling`: the leader's backstop for missed webhooks. `interval` is how
  often it lists each connection's open pull requests, 10 minutes unless
  set, and `0s` turns it off; `lookback` bounds how far back a first or
  long-idle poll looks, 24 hours unless set.
- `indexing.onboardWindow`: how many onboarding index jobs the leader keeps
  queued or running at once, 4 unless set.
- `tools`: command-line tools a runner pod mounts from an image for the
  agent's run tool ([ADR-0011](adr/0011-runner-tool-images.md)), each a
  `name`, a digest-pinned `image`, the `path` of its binaries and the
  `commands` it provides.
- `retention`: `disabledIndexGrace`, how long the index of a repository
  that stopped running is kept, 30 days unless set; and `transcripts`, how
  long an agentic review's full model transcript is kept, 30 days unless
  set and at least 24 hours. A transcript may contain repository content
  the agent read, and every member of its account can read it.
- `egress`: what runner pods may reach through the worker's gateway beyond
  the forges: `allowHosts`, exact or `*.`-prefixed, and `credentials`, a
  token the gateway adds to a plain `http://` request to that host.
