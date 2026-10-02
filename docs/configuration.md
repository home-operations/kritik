# Configuration

kritika takes its settings from three places, each for what it suits:

| Where                                                                                                                                               | What                                                                                                                                             | Changed by                         |
| --------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- |
| The environment                                                                                                                                     | Process wiring: addresses, the database, logging and `KRITIKA_WEB_URL`                                                                           | a restart                          |
| The configuration file, and its `KRITIKA_AUTH_*`, `KRITIKA_APPS_*`, `KRITIKA_PROVIDERS_*`, `KRITIKA_DEFAULTS_*` and `KRITIKA_EMBEDDING_*` variables | What is reviewed and how: sign-in, the GitHub Apps, model providers, the embedder, egress, the defaults, the repository entries and the accounts | a restart                          |
| The environment                                                                                                                                     | How kritika runs: polling, onboarding, retention and runner Jobs                                                                                 | a restart                          |
| The dashboard                                                                                                                                       | Whether each repository is on or off                                                                                                             | an admin, on the Repositories page |

A repository's own [`.kritika.yaml`](repository-config.md) narrows what the
configuration sets for it, from its own git.

The file is optional: `KRITIKA_CONFIG_FILE` names it, and the chart's
`configFile` renders it, so the configuration lives in git with the rest
of the deployment. In the file a secret is `{ env: NAME }`, the variable
holding it, never the value itself; the chart's `env` and `envFrom` set
such a variable from an existing Secret. Sign-in, one app, one provider, the
default models and review settings, and the embedder also have
variables, and a variable wins over the file, so a small deployment can
be configured from the environment alone. A variable carries a secret
itself. The chart's `config` has a key for every `KRITIKA_*` variable, its
name without the prefix in camelCase (`KRITIKA_AUTH_OIDC_ISSUER` is
`authOidcIssuer`), a secret's taking a `valueFrom` from a Secret. Once the configuration is read, kritika drops every variable a
secret came from from its own environment. A variable under
one of these prefixes that names no key is refused at startup rather than
ignored.

A key whose value is a [CEL](https://cel.dev) expression ends in `Expr`:
`filterExpr`, `roleMappingExpr` and a rule's `whenExpr`.

kritika reads the file and its variables once, at startup: a change takes a
restart, and the chart rolls the pods when its `configFile` changes.
Content that does not load, or that would leave the dashboard no way to
sign in, fails startup, so a rolling update leaves the pods before it
serving.

## `auth`

`auth` sets how people sign in and what each may do. Its variables are
keys of the chart's `config`, the secrets among them from existing Secrets.

| Key                      | Environment variable                        |
| ------------------------ | ------------------------------------------- |
| `sessionTTL`             | `KRITIKA_AUTH_SESSION_TTL`                  |
| `admin.user`             | `KRITIKA_AUTH_ADMIN_USER`                   |
| `admin.password`         | `KRITIKA_AUTH_ADMIN_PASSWORD`               |
| `oidc.name`              | `KRITIKA_AUTH_OIDC_NAME`                    |
| `oidc.issuer`            | `KRITIKA_AUTH_OIDC_ISSUER`                  |
| `oidc.clientId`          | `KRITIKA_AUTH_OIDC_CLIENT_ID`               |
| `oidc.clientSecret`      | `KRITIKA_AUTH_OIDC_CLIENT_SECRET`           |
| `oidc.scopes`            | `KRITIKA_AUTH_OIDC_SCOPES`, comma-separated |
| `oidc.rolesClaim`        | `KRITIKA_AUTH_OIDC_ROLES_CLAIM`             |
| `oidc.roleMappingExpr`   | `KRITIKA_AUTH_OIDC_ROLE_MAPPING_EXPR`       |
| `oidc.defaultRole`       | `KRITIKA_AUTH_OIDC_DEFAULT_ROLE`            |
| `github.clientId`        | `KRITIKA_AUTH_GITHUB_CLIENT_ID`             |
| `github.clientSecret`    | `KRITIKA_AUTH_GITHUB_CLIENT_SECRET`         |
| `github.roleMappingExpr` | `KRITIKA_AUTH_GITHUB_ROLE_MAPPING_EXPR`     |

```yaml
auth:
  admin:
    password: { env: ADMIN_PASSWORD }
  oidc:
    name: Company SSO
    issuer: https://idp.example.com
    clientId: kritika-dashboard
    clientSecret: { env: OIDC_CLIENT_SECRET }
    scopes: [openid, email, profile]
    rolesClaim: groups
    roleMappingExpr: '"kritika-admins" in roles ? "admin" : ("kritika-users" in roles ? "member" : "")'
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
  App's own: the App kritika reviews through can serve both, with a client
  secret generated on its settings page (see [setup](setup.md)).
- `sessionTTL` is how long a dashboard session lasts, between 5 minutes and
  30 days. It defaults to 12 hours.

A provider must allow the callback URL `<KRITIKA_WEB_URL>/auth/callback/oidc`
or `<KRITIKA_WEB_URL>/auth/callback/github`. The dashboard refuses to start
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
  configuration file, or kritika's default. A secret shows only whether it
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

`apps` declares the GitHub Apps kritika serves accounts through. The
[setup guide](setup.md) covers creating one.

```yaml
apps:
  github:
    accounts: [org-1, user-1]
    clientId: Iv1.example
    privateKey: { env: GITHUB_APP_PRIVATE_KEY }
    webhookSecret: { env: GITHUB_APP_WEBHOOK_SECRET }
```

`apps` is keyed by app name, which is the App's webhook path,
`/hooks/<name>`; `accounts` are the users and organizations it serves: a webhook for any other is ignored, and an
account is served by one app. `clientId` is given inline or, like the
secrets, as a reference. `webhookSecret` holds the same value as the App's
webhook secret. The same app can come from the environment instead:

| Variable                      | Key                          |
| ----------------------------- | ---------------------------- |
| `KRITIKA_APPS_NAME`           | the key, `github` unless set |
| `KRITIKA_APPS_ACCOUNTS`       | `accounts`, comma-separated  |
| `KRITIKA_APPS_CLIENT_ID`      | `clientId`                   |
| `KRITIKA_APPS_PRIVATE_KEY`    | `privateKey`                 |
| `KRITIKA_APPS_WEBHOOK_SECRET` | `webhookSecret`              |

The environment declares at most one app. It replaces the file's app of
the same name whole, or is added to the file's when none has that name. A
`KRITIKA_APPS_*` variable that names no key is refused at startup.

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
`baseUrl`, `pricing` and `retries`, and its `apiKey`. A model is named
`<provider>/<model>`, on a provider the file declares.

`retries` is how many more times a review's model step is tried when the
provider fails it in a way another attempt may not: a 5xx, a 429, a
timeout or a cut connection. The gateway waits a second, then twice as
long each time, up to 30 seconds; it never retries a refusal of the
request itself, such as a prompt over the model's input limit, or a spent
budget. It is 0 unless set, one attempt, and at most 5. A routing proxy
that picks a model per request is where it earns its keep: a step the
proxy routed badly is answered on the next attempt. Follow-ups and the
embedder do not retry.

The embedder's `model` is a model of an `openrouter` or `openai` provider
of the instance, whose endpoint, or the type's default, and key it uses;
`dims` is its dimension, at most 4000, and the optional `maxBatch`,
`maxBatchChars` and `maxItemChars` bound one request to 64 inputs, 200,000
characters and 16,000 characters per input unless set. With one, a
review's prompt carries the index's chunks nearest the change and the
agent gets a `search_code` tool over the same index; both keep a chunk
whose cosine similarity to the query is at least `similarFloor`, 0.5
unless set. Where useful matches part from noise depends on the model and
the repository: a small embedder on a repository of similar files can
score nearly everything above 0.5, and a floor of about 0.7 keeps the
matches that mean something. Without an
embedder, indexing is off and reviews run without similar code. The index
holds one model and dimension: a configuration that changes either drops
every repository's index, and the leader builds each again, a few at a
time, as `KRITIKA_ONBOARD_WINDOW` paces them. Removing the embedder keeps
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
- The model must support tool calls: a review works through tools and
  submits its findings as a call to `submit_review`, which its last step
  tells it to make, and a follow-up's reply is a tool call too.
- The kritika pods call the server; a review's runner reaches it only
  through their gateway. With the chart's `networkPolicy.enabled`,
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
account, and `owner/name` for one.

A `fallback` on the review model's provider is handed to the provider
with the request, as OpenRouter's server-side fallback is, and the
provider or the adapter tries it when the review model fails. A fallback
on another provider is tried by the gateway itself: once a review's step
has failed on the review model, and its provider's `retries` are spent,
the same step goes to the fallback, with that provider's own `retries`,
and the review carries on there. The step's usage is recorded under the
model that answered. A follow-up uses a fallback on its own provider
alone.

```yaml
defaults:
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

Each takes the keys a repository's own `.kritika.yaml` takes, at the same
level (`models`, `feedback`, `comments`, `requireSuggestedFix`,
`approve`, `filterExpr`, `ignore`, `rules`, `context` and `agentFiles`; see
[the repository settings](repository-config.md)), and the admin's own:

- `agent`: a review's `maxSteps`, `maxToolOutputBytes`, `maxTokens`,
  `timeout`, the `commands` its run tool may execute, and their
  `commandTimeout`. `maxSteps: 1` is the cheapest review: one call, which
  must submit the findings, over the same prompt. The runner's `-tools` image has `gh`, `curl`,
  `fd`, `jq`, `rg` and `yq`; the agent is told to use `gh` for GitHub, which signs in
  with a token minted for the run that can only read the repository under
  review and public repositories.
- `settle`: how long a new head waits before its review starts, so a
  burst of pushes is reviewed once.
- `maxAutoReviews`: how many automatic reviews a pull request gets before
  kritika pauses them, so a long-lived pull request stops spending on
  every push; unlimited unless set. The summary of the last one says so.
  `@<app slug> review` still reviews a paused pull request, and
  `@<app slug> resume` turns its automatic reviews back on, as
  `@<app slug> pause` turns them off at any time.
- `maxChangedLines`: the most lines a pull request's diff may add and
  remove, paths the `ignore` globs match left out, for an automatic review
  to run; a larger one is skipped before any model is called, and the
  commit status says so. Unlimited unless set. `@<app slug> review` reviews
  it anyway.
- `forks: true`: reviews pull requests from forks without being asked; by
  default one is reviewed only when a maintainer comments
  `@<app slug> review`.
- `incremental.maxDeltaFiles`: how many files may change since the last
  review before a re-review covers the whole pull request again. A re-run
  at the head the last review saw always covers the whole pull request.
- `enabled`, at `defaults` and `owner/*` only: where repositories start
  (see below).

A value applies in this order: kritika's default, `defaults`, `owner/*`,
`owner/name`, and the repository's `.kritika.yaml`. A narrower value
replaces the broader one's, except `ignore` globs, which add up, and
`rules`, which add up by id.

The same defaults can come from the environment:

| Variable                           | Key                                                                                   |
| ---------------------------------- | ------------------------------------------------------------------------------------- |
| `KRITIKA_PROVIDERS_NAME`           | the provider's name, `openrouter` unless set                                          |
| `KRITIKA_PROVIDERS_TYPE`           | `type`, which defaults to the name when that is `openrouter`, `openai` or `anthropic` |
| `KRITIKA_PROVIDERS_BASE_URL`       | `baseUrl`                                                                             |
| `KRITIKA_PROVIDERS_API_KEY`        | `apiKey`                                                                              |
| `KRITIKA_PROVIDERS_RETRIES`        | `retries`                                                                             |
| `KRITIKA_DEFAULTS_MODELS_REVIEW`   | `defaults.models.review`                                                              |
| `KRITIKA_DEFAULTS_MODELS_FALLBACK` | `defaults.models.fallback`                                                            |
| `KRITIKA_DEFAULTS_FEEDBACK`        | `defaults.feedback`                                                                   |
| `KRITIKA_DEFAULTS_FORKS`           | `defaults.forks`, `true` or `false`                                                   |
| `KRITIKA_DEFAULTS_SETTLE`          | `defaults.settle`, a duration such as `30s`                                           |
| `KRITIKA_EMBEDDING_MODEL`          | `embedding.model`                                                                     |
| `KRITIKA_EMBEDDING_DIMS`           | `embedding.dims`                                                                      |

The environment declares at most one provider, which replaces the file's
of the same name whole or is added to the file's. The embedding variables
set the file's embedder key by key. The Configuration page lists what the
file and the environment set under "Instance settings", with where each
comes from.

### Which repositories run

kritika registers every repository each App reaches, once the
configuration is applied and again on every poll, and "Resync from GitHub"
on the Repositories page does the same at once. Whether one runs is
decided in this order:

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
once `KRITIKA_INDEX_GRACE` has passed.

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
  `.kritika.yaml`, runs on that key and the account pays for it; a key's
  name may not be one the instance's providers use.

An account runs while an app serves it. An entry for an account no app
serves is kept, but not run, and the Configuration page lists it as not
served.

## `egress`

`egress` is what runner pods may reach through kritika's gateway beyond
`github.com` and `api.github.com`, which an app allows: `allowHosts`,
exact or `*.`-prefixed, and `credentials`, a token the gateway adds to a
plain `http://` request to that host, so the runner never holds it.

## How kritika runs

These come from the environment rather than the file, each a camelCased
key of the chart's `config` (`KRITIKA_POLL_INTERVAL` is `pollInterval`)
except where noted; a restart changes them.

| Variable                           | What                                                                                                                                    |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `KRITIKA_POLL_INTERVAL`            | how often the leader lists each app's open pull requests, its backstop for missed webhooks; `0s` turns it off; 10m unless set           |
| `KRITIKA_POLL_LOOKBACK`            | how far back a first or long-idle poll looks; 24h unless set                                                                            |
| `KRITIKA_ONBOARD_WINDOW`           | how many onboarding index jobs the leader keeps queued or running at once; 4 unless set                                                 |
| `KRITIKA_INDEX_GRACE`              | how long the index of a repository that stopped running is kept; 720h unless set                                                        |
| `KRITIKA_TRANSCRIPT_RETENTION`     | how long a review's full model transcript is kept, at least 24h; 720h unless set                                                        |
| `KRITIKA_DIFF_RETENTION`           | how long a review keeps the diff it was made from, the context it read and the repository files it named, at least 24h; 720h unless set |
| `KRITIKA_REVIEW_WORKERS`           | review jobs one replica runs at once; 2 unless set                                                                                      |
| `KRITIKA_INDEX_WORKERS`            | index jobs one replica runs at once; 1 unless set                                                                                       |
| `KRITIKA_RUNNER_DEADLINE`          | a runner Job's deadline; 15m unless set                                                                                                 |
| `KRITIKA_RUNNER_RUNTIME_CLASS`     | the RuntimeClass of runner Jobs, e.g. `gvisor`; the cluster default unless set                                                          |
| `KRITIKA_RUNNER_IMAGE_PULL_POLICY` | the runner container's imagePullPolicy, `Always`, `IfNotPresent` or `Never`; the chart's `runner.image.pullPolicy` renders it           |
| `KRITIKA_RUNNER_RESOURCES`         | a runner pod's resources, as JSON; the chart's `runner.resources` renders it                                                            |
| `KRITIKA_RUNNER_TOOLS`             | command-line tools a runner pod mounts from an image for the agent's run tool, as JSON; the chart's `runner.tools` renders it           |

A transcript may contain repository content the agent read, and every
member of its account can read it. A review past the diff retention keeps
its findings, summary and what it read by name and size; the dashboard's
diff and raw views say the bodies were not kept. A tool is a `name`, a digest-pinned
`image`, the `path` of its binaries and the `commands` it provides.
