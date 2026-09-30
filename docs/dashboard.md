# Dashboard

kritik serves a dashboard: sign in with a local admin password,
GitHub or an OIDC provider, and see the accounts you can read, the
connection serving each and its repositories, live review and conversation state as it
runs, and, for an admin, the audit log. An admin can also queue a re-run
of a specific pull request, cancel a review in progress, or reindex a
repository's embeddings, from the dashboard rather than the forge.

The top bar switches between the accounts you can read, or all of them
at once, and holds a tab for each of an account's sections
([ADR-0017](adr/0017-dashboard-sections.md)):

- **Analytics:** the account's reviews over the last 7, 30 or 90 days
  against the same span before: pull requests reviewed, reviews,
  findings, the share addressed, the median review time, the median time
  from opening to merging, the 👍 and 👎 on kritik's inline comments and
  spend, by day or week, and the most reviewed repositories. The poller
  reads reactions, for a week after a pull request's latest review, so
  they need `polling` on. Its Findings list has
  each finding once per pull request however many reviews repeated it,
  addressed once a later review of the pull request, at a newer head, no
  longer reports it. A finding kritik posted inline links to its thread
  on GitHub, here and on its review. Spend has the month so far against the account's
  caps, and usage by day, model, repository or role.
- **Pull requests:** its pull requests and their reviews, the run queue
  and the follow-up questions. The search box takes text, or narrows the
  list with `repo:owner/name`, `author:login` and `status:` a last review
  status, and suggests each as you type. An admin can pick pull requests,
  by checkbox or with Space on the keyboard's row, and re-run them
  together.
- **Rules:** what its reviews check: rules written in the configuration
  ([ADR-0018](adr/0018-rules.md)), instruction files they follow and
  context files that explain the code, each with where it is set (a layer
  of the configuration, a repository's entry, or a repository's
  `.kritik.yaml` as its last review read it), the paths it applies to,
  and the repositories that read it. The page only lists them.
- **Settings:** its repositories, and for an admin its audit log and the
  instance's Configuration page.

A dot in the top bar shows whether live updates are connected. Once they
have been down for two seconds it reads "Reconnecting…", and the page may
be out of date until they are back.

It is served at `KRITIK_WEB_URL`, the chart's `web.url`, which the webhook
listener shares under `/hooks`. People sign in as
[`auth`](configuration.md#auth) configures, with the role it maps them to.

## First run

The dashboard does not configure kritik: the configuration file does
([ADR-0019](adr/0019-configuration-in-git.md)). Until an instance can
review, with a running connection and a default review model, a banner
tells an admin so and leads to the Configuration page, whose Setup
checklist names each step still missing and what to set for it:

1. **A GitHub App is connected:** declared under `connections`.
2. **The App reaches a repository:** installed on an account its
   connection lists.
3. **A review model is set:** `defaults.models.review`.
4. **An embedder is set:** `embedding`, which is optional.

## Configuration page

An admin's Configuration page, under Settings, shows what the instance
runs and changes none of it: the Setup checklist, the accounts the
connections serve, each instance setting with its source, the stored
instance configuration, the connections with the accounts each App is
installed on, and the admin audit log. The command palette, `Ctrl`/`⌘`
`K`, finds each of those sections, and the Settings navigation lists them
while the page is open. A repository's page filters its effective
settings.

## Instance configuration

Everything but sign-in and the file's connections is the instance
configuration: one document kept in Postgres and written through
`PUT /api/v1/config`, until it moves into the configuration file
([ADR-0019](adr/0019-configuration-in-git.md)). It holds the connections added in the dashboard, the instance's
provider keys, its `embedding`, `defaults`, `polling`, `indexing`, `tools`,
`retention`, `egress`, and `accounts`, each account's own settings,
provider keys and repository entries. The configuration file may set the
instance's providers, default models and embedder as well
([instance defaults](configuration.md#instance-defaults-providers-defaults-and-embedding)):
the instance configuration's own, where it sets them, override the
file's.

- The Configuration page shows the document read-only, each secret as
  set or not; the dashboard has no form for it.
- A save names the revision it was loaded at. A save over a newer
  revision is refused with `409 revision_conflict`.
- A save that would not run is refused with `422`, naming the offending
  key. Every replica picks up a saved revision through Postgres `NOTIFY`.

An account entry is keyed by forge and name, and a repository entry names
the repository without its owner:

```json
{
  "accounts": [
    {
      "forge": "github",
      "name": "org-1",
      "models": { "review": "openrouter/openai/gpt-6-sol" },
      "limits": { "reviewsPerDay": 50 },
      "repositories": [{ "name": "repo-1", "mode": "agentic" }]
    }
  ]
}
```

An account runs while a connection serves it. An entry for an account no
connection serves is kept, but not run, and the Configuration page lists
it as not served.

Every repository the App reaches is reviewed and indexed unless it is
turned off. `enabled` turns it off or on, and like the other settings it
can be written at the defaults, an account or a repository entry. At the
defaults or an account, it is where each repository without an entry of
its own starts: with `"enabled": false` on the account, a repository the
App reaches is registered but nothing runs for it until its own entry says
`"enabled": true`. A repository that is off is neither polled nor
indexed.

Forks and archived repositories are the exceptions. A fork only runs once
its own entry says `"enabled": true`, whatever the defaults and the
account say, since an account can reach many forks it never meant to
review. An archived repository never runs: unarchive it on GitHub first.
kritik learns both from GitHub: from the App's repository listing, from
the repository each pull request, comment and push webhook names, and
from the App's repository events, which say when one is archived or
unarchived.

An admin switches repositories on and off on an account's Repositories
page, one at a time or a selection together, and reindexes a selection
from there too. A switch is the dashboard's own choice, kept per
repository and audited ([ADR-0019](adr/0019-configuration-in-git.md)):
it wins over `enabled` and over a fork's own entry, and only an archived
repository stays off whatever it says. A repository no admin has switched
runs as the configuration says. The page lists the repositories that can
run, with any fork turned on; its Type filter lists the forks, or the
archived repositories, instead. kritik registers every repository each connection's App reaches
by itself: once the configuration is applied, at start or after a change,
and again on every poll, so a fresh instance lists them without a
webhook. "Resync from GitHub" does the same at once, such as right
after unarchiving one. An account's repository count is of the ones that
run.

## Secrets

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
is not running, is a `409 Conflict`.

## Provider keys

An account can bring its own model keys: `providers` in its entry, the
same shape as the instance's `providers`, edited in the "Provider keys"
section of the account's Configuration page. A model named `<key name>/<model>`
then runs on that key, and the account pays for it; a key's name may not
be one the instance's providers already use. The account's review and fallback models,
and a repository entry's, may name a model on one of these keys. The keys are sealed at rest like connection secrets
and never shown again. A saved key is kept only while its name, type and
endpoint stay the same, so a key cannot be sent anywhere it was not
entered for. Account limits still apply to runs on an account's own key.

"Test key", beside any provider key or the embedder, checks a key before
it is saved with one cheap call: listing the provider's models, checking
an OpenRouter key against its key endpoint, or embedding one word at the
embedder's dimension. The provider's answer is shown as it came.

## Embeddings

The embedder builds each repository's similar-code index, which reviews
draw context from. It is the instance configuration's `embedding`: any
OpenAI-compatible embeddings endpoint, with `baseUrl`, `apiKey`, `model`
and `dims`, at most 4000. The optional `maxBatch`, `maxBatchChars` and
`maxItemChars` bound one request, 64 inputs, 200,000 characters and 16,000
characters per input unless set. Without an embedder, indexing is off and
reviews run without vector retrieval.

The index holds one model and dimension. A save that changes either is
refused with `409 reindex_required` until the admin confirms the reindex.
The leader then drops every repository's index and builds each again, a
few at a time, as `indexing.onboardWindow` paces them. Removing the
embedder keeps the index, and adding back the same model and dimension
uses it again. The key is kept only while `baseUrl` stays the same.

## Sealing key

The instance configuration's secrets are sealed at rest with an instance
key, `KRITIK_DASHBOARD_KEY` / `dashboard.keySecret`: generate one with
`openssl rand -base64 32`. Without it the admin console is read-only, and
kritik refuses to start once a configuration is stored. To rotate it, move the old value into
`KRITIK_DASHBOARD_OLD_KEYS` / `dashboard.oldKeysSecret` (comma-separated,
accepted only to open values already sealed under it), and set a freshly
generated value as `KRITIK_DASHBOARD_KEY`. Every save of the configuration
re-seals the values still under an old key with the current one, so after a
rotation save the instance configuration once from the admin console, even
unchanged, and then the old key can be dropped.

## `retention.transcripts`

The instance configuration's `retention.transcripts` (default 30 days,
minimum 24 hours)
controls how long an agentic review's full model transcript is kept; the
review itself, its findings and its comments outlive it. A transcript may
contain repository content the agent read while investigating, and it is
visible to every member of the account it belongs to, not only admins.

## Operational notes

- An instance configuration that fails to merge with the file at boot
  fails startup the same as a bad configuration file: fix the file, or
  the stored configuration. A merge or apply failure after boot instead
  keeps the last good configuration running and raises the
  `kritik_config_error` gauge (labelled `merge` or `apply`) until a later
  attempt succeeds.
- A file connection whose name or account a dashboard connection already
  holds is left out of the running configuration, at boot or on reload,
  while everything else runs: the Configuration page lists it with the reason,
  and `kritik_config_error{stage="merge"}` stays at 1. Rename either side,
  or remove the dashboard connection, to bring it back.
- A secret referenced by `file:` is only re-read when the configuration
  file itself changes, not on the referenced file's own schedule: rotate
  the file, then touch or reapply the configuration to pick it up.
- A role mapping is only as trustworthy as what it reads. Map on groups
  or roles the IdP controls, not on an email or name a user can set on
  their own profile.
- The web role only ever holds the application database DSN, never the
  owner DSN a migration or leader election needs, and refuses to start if
  it would.
