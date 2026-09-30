# Configuration

kritik takes its settings from three places, each for what it suits
([ADR-0019](adr/0019-configuration-in-git.md)):

| Where                                                                                                                                          | What                                                                                                                                             | Changed by                         |
| ---------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- |
| The environment                                                                                                                                | Process wiring: addresses, the database, logging and `KRITIK_WEB_URL`                                                                            | a restart                          |
| The configuration file, and its `KRITIK_AUTH_*`, `KRITIK_APPS_*`, `KRITIK_PROVIDERS_*`, `KRITIK_DEFAULTS_*` and `KRITIK_EMBEDDING_*` variables | What is reviewed and how: sign-in, the GitHub Apps, model providers, the embedder, egress, the defaults, the repository entries and the accounts | a restart                          |
| The environment, set by the chart's values                                                                                                     | How kritik runs: polling, onboarding, retention and runner Jobs                                                                                  | a restart                          |
| The dashboard                                                                                                                                  | Whether each repository is on or off                                                                                                             | an admin, on the Repositories page |

A repository's own [`.kritik.yaml`](repository-config.md) narrows what the
configuration sets for it, from its own git.

The file is optional: `KRITIK_CONFIG_FILE` names it, and the chart's
`config.file` renders it, so the configuration lives in git with the rest
of the deployment. In the file a secret is `{ env: NAME }`, the variable
holding it, never the value itself; the chart's `secretEnv` sets such a
variable from an existing Secret. Sign-in, one app, one provider, the
default models and review settings, and the embedder also have
variables, and a variable wins over the file, so a small deployment can
be configured from the environment alone. A variable carries a secret
itself. Once the configuration is read, kritik drops every variable a
secret came from from its own environment
([ADR-0022](adr/0022-configuration-at-startup.md) §2.2). A variable under
one of these prefixes that names no key is refused at startup rather than
ignored.

A key whose value is a [CEL](https://cel.dev) expression ends in `Expr`:
`filterExpr`, `roleMappingExpr` and a rule's `whenExpr`.

kritik reads the file and its variables once, at startup
([ADR-0022](adr/0022-configuration-at-startup.md)): a change takes a
restart, and the chart rolls the pods when its `config.file` changes.
Content that does not load, or that would leave the dashboard no way to
sign in, fails startup, so a rolling update leaves the pods before it
serving.

## `auth`

`auth` sets how people sign in and what each may do. The chart's `auth`
values render its variables.

| Key                      | Environment variable                       |
| ------------------------ | ------------------------------------------ |
| `sessionTTL`             | `KRITIK_AUTH_SESSION_TTL`                  |
| `admin.user`             | `KRITIK_AUTH_ADMIN_USER`                   |
| `admin.password`         | `KRITIK_AUTH_ADMIN_PASSWORD`               |
| `oidc.name`              | `KRITIK_AUTH_OIDC_NAME`                    |
| `oidc.issuer`            | `KRITIK_AUTH_OIDC_ISSUER`                  |
| `oidc.clientId`          | `KRITIK_AUTH_OIDC_CLIENT_ID`               |
| `oidc.clientSecret`      | `KRITIK_AUTH_OIDC_CLIENT_SECRET`           |
| `oidc.scopes`            | `KRITIK_AUTH_OIDC_SCOPES`, comma-separated |
| `oidc.rolesClaim`        | `KRITIK_AUTH_OIDC_ROLES_CLAIM`             |
| `oidc.roleMappingExpr`   | `KRITIK_AUTH_OIDC_ROLE_MAPPING_EXPR`       |
| `oidc.defaultRole`       | `KRITIK_AUTH_OIDC_DEFAULT_ROLE`            |
| `github.clientId`        | `KRITIK_AUTH_GITHUB_CLIENT_ID`             |
| `github.clientSecret`    | `KRITIK_AUTH_GITHUB_CLIENT_SECRET`         |
| `github.roleMappingExpr` | `KRITIK_AUTH_GITHUB_ROLE_MAPPING_EXPR`     |

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
    roleMappingExpr: '"kritik-admins" in roles ? "admin" : ("kritik-users" in roles ? "member" : "")'
  github:
    clientId: Iv1.abc123
    clientSecret: { env: GITHUB_CLIENT_SECRET }
    roleMappingExpr: 'login == "user-1" ? "admin" : ""'
```

- `admin` is the local admin. It signs in on the sign-in page with a
  username, `admin` unless `user` sets another, and `password`. It exists
  only while a password is set: it is the way into a fresh instance, and a
  way in when every provider is down. Ten failed attempts from one address
  within 15 minutes lock that address out until the window passes; each
  replica counts its own.
- `oidc` signs in through any OpenID Connect issuer, an `https` URL. The
  sign-in page labels it `name`, or "SSO" when unset.
- `github` signs in on github.com with an OAuth App's client, or a GitHub
  App's own: the App kritik reviews through can serve both, with a client
  secret generated on its settings page (see [setup](setup.md)).
- `sessionTTL` is how long a dashboard session lasts, between 5 minutes and
  30 days. It defaults to 12 hours.

A provider must allow the callback URL `<KRITIK_WEB_URL>/auth/callback/oidc`
or `<KRITIK_WEB_URL>/auth/callback/github`. The dashboard refuses to start
with no way to sign in. The configuration is refused when nothing could
make an admin: set an admin password, or a `roleMappingExpr` on a
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

A `roleMappingExpr` is a [CEL](https://cel.dev) expression evaluated at
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

## `apps`

`apps` declares the GitHub Apps kritik serves accounts through
([ADR-0021](adr/0021-configuration-shape.md) §2.2). The
[setup guide](setup.md) covers creating one.

```yaml
apps:
  - name: github
    accounts: [org-1, user-1]
    clientId: Iv1.example
    privateKey: { env: GITHUB_APP_PRIVATE_KEY }
    webhookSecret: { env: GITHUB_APP_WEBHOOK_SECRET }
```

`name` is the App's webhook path, `/hooks/<name>`, and `accounts` the users
and organizations it serves: a webhook for any other is ignored, and an
account is served by one app. `clientId` is given inline or, like the
secrets, as a reference. `webhookSecret` holds the same value as the App's
webhook secret. The same app can come from the environment instead:

| Variable                     | Key                         |
| ---------------------------- | --------------------------- |
| `KRITIK_APPS_NAME`           | `name`, `github` unless set |
| `KRITIK_APPS_ACCOUNTS`       | `accounts`, comma-separated |
| `KRITIK_APPS_CLIENT_ID`      | `clientId`                  |
| `KRITIK_APPS_PRIVATE_KEY`    | `privateKey`                |
| `KRITIK_APPS_WEBHOOK_SECRET` | `webhookSecret`             |

The environment declares at most one app. It replaces the file's app of
the same name whole, or is added to the file's when none has that name. A
`KRITIK_APPS_*` variable that names no key is refused at startup.

## `providers` and `embedding`

`providers` are the instance's model keys, and `embedding` the embedder
that builds each repository's similar-code index from one of them.

```yaml
providers:
  openrouter:
    type: openrouter
    apiKey: { env: OPENROUTER_API_KEY }
embedding:
  model: openrouter/voyageai/voyage-code-4
  dims: 1024
```

A provider is `type` (`openrouter`, `openai` or `anthropic`), an optional
`baseUrl` and `pricing`, and its `apiKey`. A model is named
`<provider>/<model>`, on a provider the file declares.

The embedder's `model` is a model of an `openrouter` or `openai` provider
of the instance, whose endpoint, or the type's default, and key it uses;
`dims` is its dimension, at most 4000, and the optional `maxBatch`,
`maxBatchChars` and `maxItemChars` bound one request to 64 inputs, 200,000
characters and 16,000 characters per input unless set. Without an
embedder, indexing is off and reviews run without similar code. The index
holds one model and dimension: a configuration that changes either drops
every repository's index, and the leader builds each again, a few at a
time, as `KRITIK_ONBOARD_WINDOW` paces them. Removing the embedder keeps
the index, and adding back the same model and dimension uses it again.

### Local models

A model served in your own network is a provider of type `openai` whose
`baseUrl` is the server's OpenAI-compatible API, such as vLLM, Ollama or
llama.cpp's server, or of type `anthropic` for a server that speaks the
Anthropic API. It can serve reviews, the embedder, or both:

```yaml
providers:
  local:
    type: openai
    baseUrl: http://llm.example.svc.cluster.local:8000/v1
    apiKey: { env: LOCAL_LLM_KEY }
    pricing:
      large-model: { input: 0.1, output: 0.4 }
defaults:
  models: { review: local/large-model }
embedding:
  model: local/embed-model
  dims: 768
```

- `apiKey` is required: a server that takes no key still needs a
  reference, to a variable holding any value.
- The model must support tool calls, and the server must honour a
  request that forces a named one: kritik asks for a review's findings as
  a call to a tool named for their schema, and an agentic review works
  through tools.
- The kritik pods call the server; an agentic review's runner reaches it
  only through their gateway. With the chart's `networkPolicy.enabled`,
  add the server's port to `networkPolicy.egressPorts`, which allows only
  443 unless set.
- A server that reports no cost makes every call cost nothing unless
  `pricing` gives the model's prices, in dollars per million tokens of
  `input`, `output`, `cacheRead` and `cacheWrite`, keyed by the model's
  id on the server. Tokens count against an account's `limits` either
  way.
- The embedder's `dims` must be what its model returns.

## `defaults` and `repositories`

`defaults` are the settings every repository gets, and `repositories` the
entries that change them for some: `owner/*` for every repository of an
account, and `owner/name` for one
([ADR-0021](adr/0021-configuration-shape.md) §2.1).

```yaml
defaults:
  mode: agentic
  models: { review: openrouter/vendor/large-model, fallback: openrouter/vendor/small-model }
  feedback: detailed
  settle: 30s
  rules:
    - { id: no-tokens, rule: "Never log a token, key or password." }
    - id: renovate
      rule: Say what the update breaks, from the release notes in the body.
      whenExpr: pr.headRef.startsWith("renovate/")
repositories:
  org-1/*:
    models: { review: org-1-key/vendor/large-model }
    rules:
      - {
          id: wrap-errors,
          rule: 'Wrap errors with fmt.Errorf("<package>: %w", err).',
          paths: ["**/*.go"],
        }
  org-1/repo-1:
    feedback: minimal
    agent: { commands: [gh, curl] }
```

Each takes the keys a repository's own `.kritik.yaml` takes, at the same
level (`mode`, `models`, `feedback`, `comments`, `requireSuggestedFix`,
`approve`, `filterExpr`, `ignore`, `rules`, `context` and `agentFiles`; see
[the repository settings](repository-config.md)), and the admin's own:

- `agent`: an agentic review's `maxSteps`, `maxToolOutputBytes`,
  `maxTokens`, `timeout`, the `commands` its run tool may execute, and
  their `commandTimeout`. The runner's `-tools` image has `gh`, `curl`,
  `fd` and `rg`; the agent is told to use `gh` for GitHub, which signs in
  with a token minted for the run that can only read the repository under
  review and public repositories
  ([ADR-0023](adr/0023-gh-in-runners.md)).
- `settle`: how long a new head waits before its review starts, so a
  burst of pushes is reviewed once.
- `forks: true`: reviews pull requests from forks without being asked; by
  default one is reviewed only when a maintainer comments
  `@<app slug> review`.
- `incremental.maxDeltaFiles`: how many files may change since the last
  review before a re-review covers the whole pull request again.
- `enabled`, at `defaults` and `owner/*` only: where repositories start
  (see below).

A value applies in this order: kritik's default, `defaults`, `owner/*`,
`owner/name`, and the repository's `.kritik.yaml`. A narrower value
replaces the broader one's, except `ignore` globs, which add up, and
`rules`, which add up by id ([ADR-0018](adr/0018-rules.md)). `mode` is
`agentic` unless set, which needs the chart's `gateway`.

The same defaults can come from the environment:

| Variable                          | Key                                                                                   |
| --------------------------------- | ------------------------------------------------------------------------------------- |
| `KRITIK_PROVIDERS_NAME`           | the provider's name, `openrouter` unless set                                          |
| `KRITIK_PROVIDERS_TYPE`           | `type`, which defaults to the name when that is `openrouter`, `openai` or `anthropic` |
| `KRITIK_PROVIDERS_BASE_URL`       | `baseUrl`                                                                             |
| `KRITIK_PROVIDERS_API_KEY`        | `apiKey`                                                                              |
| `KRITIK_DEFAULTS_MODELS_REVIEW`   | `defaults.models.review`                                                              |
| `KRITIK_DEFAULTS_MODELS_FALLBACK` | `defaults.models.fallback`                                                            |
| `KRITIK_DEFAULTS_MODE`            | `defaults.mode`                                                                       |
| `KRITIK_DEFAULTS_FEEDBACK`        | `defaults.feedback`                                                                   |
| `KRITIK_DEFAULTS_FORKS`           | `defaults.forks`, `true` or `false`                                                   |
| `KRITIK_DEFAULTS_SETTLE`          | `defaults.settle`, a duration such as `30s`                                           |
| `KRITIK_EMBEDDING_MODEL`          | `embedding.model`                                                                     |
| `KRITIK_EMBEDDING_DIMS`           | `embedding.dims`                                                                      |

The environment declares at most one provider, which replaces the file's
of the same name whole or is added to the file's. The embedding variables
set the file's embedder key by key. The Configuration page lists what the
file and the environment set under "Instance settings", with where each
comes from.

### Which repositories run

kritik registers every repository each App reaches, once the
configuration is applied and again on every poll, and "Resync from GitHub"
on the Repositories page does the same at once. Whether one runs is
decided in this order ([ADR-0019](adr/0019-configuration-in-git.md)):

1. an archived repository never runs: unarchive it on GitHub, then
   resync;
2. one an admin turned on or off on the Repositories page runs as they
   chose;
3. a fork does not run, since an account can reach many forks it never
   meant to review;
4. any other runs as `enabled` says, at `defaults` or `owner/*`: on unless
   one sets `enabled: false`.

An `owner/name` entry may not set `enabled`: the dashboard owns a
repository's on or off, and the configuration only says where one starts.
A repository that is off is neither reviewed, polled nor indexed. One an
admin turned off, or that the App no longer reaches, has its index dropped
once `KRITIK_INDEX_GRACE` has passed.

## `accounts`

An account is a user or organization an app serves, `github/<name>`. Its
entry under `accounts`, keyed by its name, holds what is the account's
alone:

```yaml
accounts:
  org-1:
    limits: { reviewsPerDay: 50, tokensPerMonth: 20000000 }
    providers:
      org-1-key: { type: openrouter, apiKey: { env: ORG_1_OPENROUTER_API_KEY } }
```

- `limits`: `concurrency`, how many model calls it runs at once, 2 unless
  set; `reviewsPerDay`; and `tokensPerMonth`. `defaults.limits` sets every
  account's.
- `providers`: its own model keys. A model named `<key name>/<model>` in
  its `owner/*` or `owner/name` entries, or in one of its repositories'
  `.kritik.yaml`, runs on that key and the account pays for it; a key's
  name may not be one the instance's providers use.

An account runs while an app serves it. An entry for an account no app
serves is kept, but not run, and the Configuration page lists it as not
served.

## `egress`

`egress` is what runner pods may reach through kritik's gateway beyond
`github.com` and `api.github.com`, which an app allows: `allowHosts`,
exact or `*.`-prefixed, and `credentials`, a token the gateway adds to a
plain `http://` request to that host, so the runner never holds it.

## How kritik runs

These come from the environment, which the chart's values set, rather
than the file ([ADR-0021](adr/0021-configuration-shape.md) §2.7); a
restart changes them.

| Variable                      | Chart value                  | What                                                                                                                                    |
| ----------------------------- | ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `KRITIK_POLL_INTERVAL`        | `config.pollInterval`        | how often the leader lists each app's open pull requests, its backstop for missed webhooks; `0s` turns it off; 10m unless set           |
| `KRITIK_POLL_LOOKBACK`        | `config.pollLookback`        | how far back a first or long-idle poll looks; 24h unless set                                                                            |
| `KRITIK_ONBOARD_WINDOW`       | `config.onboardWindow`       | how many onboarding index jobs the leader keeps queued or running at once; 4 unless set                                                 |
| `KRITIK_INDEX_GRACE`          | `config.indexGrace`          | how long the index of a repository that stopped running is kept; 720h unless set                                                        |
| `KRITIK_TRANSCRIPT_RETENTION` | `config.transcriptRetention` | how long an agentic review's full model transcript is kept, at least 24h; 720h unless set                                               |
| `KRITIK_DIFF_RETENTION`       | `config.diffRetention`       | how long a review keeps the diff it was made from, the context it read and the repository files it named, at least 24h; 720h unless set |
| `KRITIK_RUNNER_DEADLINE`      | `runner.deadline`            | a runner Job's deadline; 15m unless set                                                                                                 |
| `KRITIK_RUNNER_RESOURCES`     | `runner.resources`           | a runner pod's resources, as JSON                                                                                                       |
| `KRITIK_RUNNER_TOOLS`         | `runner.tools`               | command-line tools a runner pod mounts from an image for the agent's run tool ([ADR-0011](adr/0011-runner-tool-images.md)), as JSON     |

A transcript may contain repository content the agent read, and every
member of its account can read it. A review past the diff retention keeps
its findings, summary and what it read by name and size; the dashboard's
diff and raw views say the bodies were not kept. A tool is a `name`, a digest-pinned
`image`, the `path` of its binaries and the `commands` it provides.
