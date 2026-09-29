# ADR-0014: a self-hosted instance, set up in the dashboard, serving GitHub through one App

- **Status:** Proposed
- **Date:** 2026-09-28
- **Authors:** onedr0p.
- **Amended by:** [ADR-0015](0015-instance-defaults-in-the-file.md), which
  lets the file set the instance's providers, default models and embedder
  under the spec's (§2.2).
- **Supersedes:** [ADR-0013](0013-hosted-instance.md) (withdrawn: kritik is
  not run as a service for others).
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.6 (the
  configuration file), [ADR-0003](0003-forgejo-agentic-review.md) §2.1
  (Forgejo and Gitea are removed; the rest of that ADR stands),
  [ADR-0009](0009-web-dashboard.md) §2.3 to §2.5 (sign-in, membership and
  dashboard tenants), [ADR-0010](0010-configuration-layers.md) §2.1 to §2.3
  (what the file owns, and the one-owner rule applied per connection),
  and [ADR-0012](0012-github-app-manifest.md) §2.1 to §2.3 (the App is
  registered for the instance, public or private, its `client_secret` is
  shown once, and there is no separate hooks URL).

> Scope: what kritik is for its first release. One self-hosted instance,
> deployed into a cluster with a small configuration file that declares
> how people sign in and, if the admin already has one, a GitHub App;
> everything else is configured in its dashboard, which walks a fresh
> instance through setup. It reviews pull requests on github.com through
> a GitHub App it can register for itself. GitLab, Forgejo, Gitea and
> GitHub Enterprise Server are removed from the code and shown greyed out
> in the dashboard, with the forge boundary kept so one can be added
> later. This ADR is a plan: every section names what exists today, what
> changes, and what is deliberately left alone.

## 1. Context

kritik grew toward two audiences at once: an organisation running it for
itself, and a host running it for others (ADR-0013). The second pulled in
per-tenant admins, invites, sealed per-tenant provider keys, self-serve
tenants, and a policy table deciding which of two writers may set each
field. It also had the same product reach four forges before any of them
had a release.

Surveying `main` at ebcf0d2:

- **Forges.** `internal/forge/{github,forgejo,gitlab}` are three clients
  behind one `forge.Client` interface; `webhook.Parse` switches on the
  forge; `worker.BuildForge` does too; `configfile.Installation` carries
  either an `app` (GitHub) or `token`/`gitToken`/`webhookSecret` (the
  others). Forgejo and GitLab together are about 1,600 lines of client and
  1,200 of tests, plus a `forgejo`/`gitea` sign-in provider, a GitLab
  signing-token prefix, a `gitea` migration and three documentation
  sections.
- **The file owns whole tenants.** A tenant is declared either in the
  operator's YAML file or as a `dashboard_tenants` row; `configfile.Merge`
  reconciles the two, leaves out a file tenant that collides with a
  dashboard one, tracks each tenant's `Origin`, and the dashboard shows a
  file tenant read-only. `configsource` watches the file, reports drift
  between replicas and applies on the leader. Model providers live only
  in the file, except that a dashboard tenant may bring its own sealed
  keys (#89, built for ADR-0013 §2.4). Secrets in the file are references,
  `{ env: NAME }` or `{ file: path }`, resolved at load.
- **Sign-in is in the file.** `web.signIn[]` declares OIDC, GitHub,
  Forgejo and Gitea providers; `web.operators[]` is a per-identity
  allowlist; there is no local account, so a fresh instance cannot be
  opened without first registering an OAuth client somewhere.
- **Three roles.** Operators come from `web.operators`; tenant admins from
  forge organisation roles or invites; members from either.
  `internal/webapi/policy.go` decides per field which of them may write.
- **Nothing of ADR-0013 is implemented** beyond per-tenant provider keys.
  ADR-0012 (an App from a manifest) is not implemented either.
- **Tenancy is in the schema.** Every content table carries `tenant_id`
  with a row-level security policy, and the runner role's own policies
  build on the same columns.
- **CEL is already here.** `internal/prfilter` compiles CEL expressions at
  startup for pull request filters.

The decision is to serve the first audience only, with a deployment that
looks like Grafana's: the chart brings the process up with a local admin
or an identity provider and a role mapping, an admin who already has a
GitHub App declares it alongside, and everything else happens in the UI.

## 2. Decision

### 2.1 Deploying is: chart, sign-in, then the dashboard

The dashboard is no longer optional. The `web` role is part of `all` and
`KRITIK_WEB_URL` is required for it, as it already is for a separate `web`
Deployment. Deploying kritik means: a database, the chart's wiring, one
public URL, and the sign-in of §2.5. The dashboard and the webhook
listener share that URL: webhooks arrive at `/hooks` under it, and the
chart's one Ingress or HTTPRoute sends `/hooks` to the listener and
everything else to the dashboard, whether one process serves both (the
`all` topology) or the ingest and web Deployments do. Everything else,
from the GitHub App to the model provider keys, is done on the dashboard
by an admin, or declared in the file where §2.2 allows it.

Two roles remain, both assigned at sign-in:

- **Admin**: administers the instance: every write in the dashboard,
  the admin console, and the instance-wide audit log. What today's code
  calls an operator.
- **Member**: read access to reviews, conversations and transcripts, and
  nothing else.

Tenant admins, invites and `email:` operators are removed; §2.5 says
where roles come from instead. Owning a GitHub organisation grants
nothing beyond membership of its account.

What each role gets in the dashboard, which is today's UI with the
tenant pages re-keyed by account (§2.4):

| Surface                                                                                                         | Member | Admin |
| --------------------------------------------------------------------------------------------------------------- | ------ | ----- |
| The accounts their role covers, with a picker when there are several; nothing about any other account           | see    | all   |
| An account's overview, repositories, pull requests, reviews (summary, diff, conversation, timeline, raw, usage) | see    | see   |
| Queue, usage and follow-ups for the account                                                                     | see    | see   |
| Each repository's effective settings and where each value came from                                             | see    | see   |
| Re-run, cancel, reindex                                                                                         |        | do    |
| Every setting: accounts and their keys, limits and repositories, defaults, providers, connections, the embedder |        | edit  |
| The setup wizard, the admin console, the instance-wide audit log                                                |        | edit  |

Read access includes an agentic review's model transcript, which can
quote repository content; today's `docs/dashboard.md` says the same. An
admin mapping OIDC members onto `"*"` is granting that across every
account, and should mean to.

### 2.2 Two layers: the file declares, the dashboard configures

ADR-0010's one-owner rule stays, applied to smaller units. The
configuration file shrinks to what an admin wants fixed at deploy
time, and one row in Postgres, `instance_config(spec jsonb, revision,
...)`, holds everything else as the JSON the dashboard's advanced editor
already speaks.

**The file holds:** `auth` (§2.5) and `connections[]` (§2.3), and
nothing else. It is optional. Secrets in it stay references,
`{ env: NAME }` or `{ file: path }`. Loading, validation, hot reload,
drift reporting and `Origin` are kept as they are.

**Every key in the file has an environment variable, and the
environment wins.** This is Grafana's `GF_<SECTION>_<KEY>` rule: a key
at path `auth.oidc.issuer` is `KRITIK_AUTH_OIDC_ISSUER`, each path
segment upper-cased with camel case split on underscores, so
`auth.oidc.roleMapping` is `KRITIK_AUTH_OIDC_ROLE_MAPPING`. A list of
scalars is comma-separated. A secret is the value itself, or the same
name with `_FILE` naming a mounted file to read it from. kritik reads
the file, if any, overlays every set variable onto the document at its
path, and validates the merged document once, so a value from either
source is checked by the same code and reported at the same path. An
admin can therefore run with no file at all, with a file and a few
variables over it, or with a file alone; the admin console already
shows each instance setting with its source, and keeps doing so for
`env`, `file` or `default`. The environment is read at startup: a file
edit hot-reloads under the same overlay, a variable change needs a
restart, as wiring does. A `KRITIK_AUTH_*` or `KRITIK_CONNECTIONS_*`
variable that maps onto no known path fails startup, so a typo is a
refusal rather than a silently ignored setting.

`connections[]` is the one list of objects. The environment declares
at most one connection, `KRITIK_CONNECTIONS_NAME` (default
`github`), `_ACCOUNTS`, `_APP_CLIENT_ID`, `_APP_PRIVATE_KEY` (or
`_APP_PRIVATE_KEY_FILE`) and `_APP_WEBHOOK_SECRET` (or `_FILE`), which
replaces the file's connection of that name whole, or is added when
the file has none by that name. A second App comes from the file or the
dashboard.

ADR-0010 §2.2's rule that no setting may be both a variable and a file
key is amended for these two keys: file and environment are one layer
with two encodings and a stated precedence, not two owners. The
dashboard spec (below) has no environment form; the leader applies it
from Postgres alone.

**The spec holds:** `providers` and their keys, `defaults`,
`connections[]` the dashboard created, `accounts[]` with each account's
settings, limits and `repositories[]` (§2.4), `polling`, `indexing`,
`retention`, `egress`, `tools` and `embedding` (§2.6). Every secret an
admin enters is sealed with `KRITIK_DASHBOARD_KEY`, bound to the
connection or provider it was entered for
(`internal/webapi/secrets.go` already does this). `configsource` merges
the file's connections into the spec as it merges dashboard tenants
into the file today, and re-reads on `NOTIFY`.

**What the dashboard may write** is the spec. A file-declared
connection is shown with its origin and is read-only, as a file tenant
is today, and a dashboard write that takes a file connection's name is
refused, as a slug collision is today. A file edit that takes a dashboard
connection's name leaves that file connection out and reports it, as
a colliding file tenant is left out today. There is no per-field overlay.

The `configfile` package keeps its types, validation and resolution
(`File.Settings`, the precedence of ADR-0010 §2.4). `Tenant` becomes
`Account` (§2.4); the file's schema loses `tenants`, `providers`,
`defaults` and the other spec-owned keys, `web.go` (replaced by
`auth.go`, §2.5), the forge values
of §2.3 and `DashboardForgeHosts`/`DashboardProviderHosts` (the admin
is the only writer, so bounding the writer bounds nobody). The
environment keeps ADR-0010 §2.2's wiring minus `KRITIK_EMBED_*`, which
moves to the spec (§2.6), and gains the file overlay above. The dashboard's
config editor, sections, field components, secret fields, generated
secrets, audit events and `WithTenant`-scoped reads are kept and
re-pointed at the spec. The policy table collapses to "admins write,
members read" and `policy.go` goes.

### 2.3 GitHub, through a GitHub App the dashboard can register

A connection is one GitHub App serving the accounts it lists, as
today: one credential (client ID, private key, webhook secret), one
webhook path `/hooks/<name>`, and the list of accounts whose
repositories it reviews. The word collides with GitHub's own, where an
"installation" is one account the App has been installed on, with its
own id and token. The two relate one-to-many: a kritik connection
holds the App, and the App has one GitHub installation per account in
`accounts`, which kritik discovers from each repository's owner and
mints tokens for separately, as `DiscoverInstallation` does today. So
`github/user-1` and `github/org-1` are two accounts, two GitHub
installations and two tenants (§2.4), served by either one kritik
connection whose App is public and lists both, or two whose Apps are
private, one registered under each. The choice is the admin's:

- **One public App, one connection, `accounts: [user-1, org-1]`.** One
  credential to rotate and one webhook to watch. A stranger can install
  the App by its slug, gets nothing (below), and is shown for removal.
- **One private App per account, one connection each.** Nobody but the
  owning account can install either App. Two credentials, two webhooks,
  and each organisation owns the App that reads its code, which is the
  governance the work instance of §2.8 wants when two unrelated
  organisations share one kritik.

`configfile.Installation` is renamed `Connection`, and so are the config
key, the `connections` table and `connection_id` columns in the squashed
migration, the hook path, the API and the UI, leaving "installation" to
GitHub's meaning alone. `Connection` keeps `name`, `forge`, `accounts`
and `app` and loses `host`, `token`, `gitToken` and `webhookSecret`. `Forge`
keeps its type and one valid value, `github`; `webhook.Parse` and
`worker.BuildForge` keep their switch with one arm. The dashboard's forge
picker lists GitHub Enterprise Server, GitLab, Forgejo and Gitea disabled
with "not yet supported", and the server refuses any other value, so a
crafted request gets the same answer as the form.

An admin who already has an App declares it in the file, or in the
`KRITIK_CONNECTIONS_*` variables of §2.2, exactly as
[connecting a forge](../connecting-a-forge.md) shows today minus the
tenant around it:

```yaml
connections:
  - name: github
    forge: github
    accounts: [example]
    app:
      clientId: Iv1.example
      privateKey: { file: /var/run/secrets/kritik/bot/private-key.pem }
      webhookSecret: { env: GITHUB_WEBHOOK_SECRET }
```

The dashboard's connections page shows it read-only, with where its
webhook must point, and offers an admin two ways to add another:

1. **Create it**, ADR-0012's manifest flow lifted from a tenant to the
   instance. kritik POSTs the manifest to GitHub with a single-use `state`
   bound to the admin's session, GitHub sends the person back with a
   `code`, and kritik converts it and stores `client_id`, `pem` and
   `webhook_secret` sealed. `hook_attributes.url` is
   `<KRITIK_WEB_URL>/hooks/<connection>`: the webhook listener is served
   under the dashboard's origin, so one public URL covers both (§2.1).
   The person picks whether the App belongs to their user or to an
   organisation, since the two register at different URLs, and whether
   it is private or public (below). Two additions to ADR-0012's manifest
   so the App can double as the GitHub sign-in of §2.5: `callback_urls`
   names `<KRITIK_WEB_URL>/auth/callback/github`, and the organisation
   permission `members: read` is requested, since a user access token may
   read `GET /user/memberships/orgs/{org}` only with it
   ([permissions required for GitHub Apps](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps)).
   The `client_secret` GitHub returns is shown once, as generated webhook
   secrets are, for the admin to put in `auth.github`; kritik does not
   keep it.
2. **Enter it**, for an App registered by hand that the admin would
   rather not put in the file: client ID, private key and webhook secret,
   with the guide's permissions and events shown beside the form. The
   webhook secret can still be generated.

Either way the page then links `https://github.com/apps/<slug>/installations/new`
and, once GitHub reports an installation, shows which accounts are served.
The conversion endpoint takes no credentials and its code lasts an hour
([create a GitHub App from a manifest](https://docs.github.com/en/rest/apps/apps#create-a-github-app-from-a-manifest)).

**Who may install the App.** An App's slug is public, and GitHub offers
no allowlist of who may install a public App, so kritik does not rely on
GitHub for that:

- The wizard asks whether the App is **private** or **public**, and
  defaults to private (`public: false`, as ADR-0012 does): GitHub then
  lets only the owning account install it, whoever knows the slug, which
  covers one account. An admin serving several accounts registers one
  private App per account, which the connections list and the wizard
  already allow, or answers public and lists every account the one App
  serves.
- `accounts` is the allowlist, and it is always explicit. A delivery for
  an account it does not list is verified, counted as
  `undeclared_account` and dropped, as today: no token is minted, no
  code is fetched, no runner or model call runs. Installing a public App
  on an account nobody listed therefore costs the admin nothing and
  reviews nothing. The wizard and the manifest flow prefill `accounts`
  with the account the App was registered under; an `installation`
  webhook from an unlisted account is shown to the admin as
  "installed, not served", and is added to `accounts` only when an
  admin says so, never on its own.
- The connections page lists every GitHub installation the App has, from
  `GET /app/installations`, marking the ones `accounts` does not cover,
  and offers to uninstall one with
  `DELETE /app/installations/{installation_id}`; both take the App's JWT
  ([GitHub Apps REST reference](https://docs.github.com/en/rest/apps/apps)).
  An uninvited installer is thus visible and removable from the
  dashboard, and the `installation` webhook raises the panel's count as
  it happens.

### 2.4 An account is the tenant

The unit of isolation is a forge account: `github/org-1`,
`github/org-2`, `github/user-1`. Nobody invents a slug: an account
exists because a connection lists it, its id derives from
`<forge>/<name>` as a tenant's derives from its slug today, and the
leader's `ApplyConfig` creates and disables the rows as it does for
tenants now. An account is served by exactly one connection, which
validation enforces, and every repository, review, finding, usage row and
transcript belongs to its account. The schema's `tenant_id` columns and
row-level security policies keep exactly their meaning under a new name.

The scopes are then `defaults`, account and repository, which is the
three-level hierarchy of ADR-0010 §2.4 with the tenant level given a
name that means something. In the spec:

```yaml
accounts:
  - forge: github
    name: org-2
    limits: { reviewsPerDay: 50, tokensPerMonth: 20000000 }
    providers:
      org-2:
        type: anthropic
        apiKey: { sealed: "..." }
    models: { review: org-2/claude-sonnet-5 }
    repositories:
      - name: repo-1
        mode: agentic
```

An account a connection serves but the spec does not list simply
inherits `defaults`. The credential that serves an account may live in
the file (§2.3) while the account's settings always live in the spec, so
the one-owner rule holds without a file-declared App freezing how its
accounts are reviewed.

**Bring your own key, per account.** `accounts[].providers` is the
per-tenant `providers` that exists today (#89), re-keyed: a model
`<name>/<model>` resolves to the account's provider of that name, else
the instance's, and a name may not be both. The gateway forwards the
call with the account's key, which never reaches a runner pod, and the
account's usage and limits count it; the key is sealed and bound to its
type and endpoint like any other. An account with no key of its own runs
on the instance's providers, so the admin's key is the default and an
organisation's key is the exception it opts into. The admin enters
and rotates the key, handed over out of band, and points the account's
models at it; there is no role for the organisation to do it itself
(§4). A repository chooses its model as ADR-0010 §2.5 already lets it:
`models.review` in `.kritik.yaml`, from the `allow.models` bound the
admin sets at `defaults`, the account or the repository entry, and
that bound may name models on the account's own key, which validation
checks resolve for that account. Embeddings stay on the instance's
embedder, since the index has one dimension per deployment (ADR-0013 §5,
which still holds).

Read access follows the account: a member sees the accounts their role
covers (§2.5), an admin sees all of them. `/api/v1/tenants/{slug}/...`
becomes `/api/v1/accounts/{forge}/{name}/...`, the tenant overview
becomes the account overview, and usage and limits are per account,
which is what bounds spend on one organisation without touching another.
In the squashed migration the `tenants` table becomes `accounts` and
every `tenant_id` column `account_id`, so the schema says what the code
means; the row-level security policies are unchanged but for the name.
The dashboard's own `accounts` table, which holds the people who sign
in, becomes `users` in the same migration, so "account" means a forge
account everywhere.

### 2.5 Sign-in: a local admin, or OIDC and GitHub with a role mapping

Sign-in is the file's `auth` block, replacing `web`, and equally its
`KRITIK_AUTH_*` variables (§2.2); at most one provider of each kind, as
Grafana has one `generic_oauth` and one `github`. The examples below use
the file form; every key in them has the variable form.

**Local admin.** `auth.admin.user` (default `admin`) and
`auth.admin.password`, a secret reference in the file or the value in
`KRITIK_AUTH_ADMIN_PASSWORD`. Set, they enable a username-and-password
form that signs in an admin; the person is a row in `users` with a
`local` identity, so audit events name them like anyone else. The
password is compared in constant time against the configured value on
each attempt, attempts are rate-limited per client address, and the
process refuses to start when no path to an admin exists: neither a
password nor a provider, or a provider with no `roleMapping`, since a
provider alone can only ever admit members. With a provider and a
mapping configured the admin is a break-glass that the chart may leave
unset, from the first launch on. The only thing GitHub sign-in needs
before kritik starts is a client ID and secret, which means registering
the App, or an OAuth App, on GitHub by hand with the callback URL set,
because the manifest flow that would do it for you runs behind an
admin session (§2.3). An admin who does that up front never sees
the password form.

**OIDC and GitHub**, in the file:

```yaml
auth:
  sessionTTL: 12h
  oidc:
    name: SSO
    issuer: https://idp.example.com
    clientId: kritik
    clientSecret: { env: OIDC_CLIENT_SECRET }
    scopes: [openid, profile, email]
    roleMapping: '"kritik-admins" in claims.groups ? "admin" : "member"'
    defaultRole: member
  github:
    clientId: Iv1.example
    clientSecret: { env: GITHUB_CLIENT_SECRET }
    roleMapping: '"org-1" in orgs ? "admin" : ""'
```

The GitHub client is a GitHub OAuth App, or the instance's own GitHub
App's client ID and secret (§2.3). The existing `oidcProvider`,
`forgeProvider` and `githubAPI` stay; only where they read their settings
changes, and the callback paths stay `/auth/callback/oidc` and
`/auth/callback/github`.

**Role mapping** is Grafana's `role_attribute_path` with CEL in place of
JMESPath, since CEL is what the codebase already compiles and smoke-tests
at startup for filters. Each mapping is one expression that yields
either an instance-wide role, `"admin"`, `"member"` or `""`, or a map
from account to `"member"`, such as `{"github/org-2": "member"}` or
`{"*": "member"}`, where `"*"` is every served account. The map form is
Grafana's `org_mapping` folded into the same expression; it is how a
sign-in that carries no forge account is scoped to some accounts rather
than all, and most instances will never write one. The expression runs:

- for OIDC over `claims`, the ID token's claims with the UserInfo
  response merged over them, and `roles`, a list of strings taken from
  the claim `auth.oidc.rolesClaim` names, whether that claim is a list or
  a map whose keys are the roles (Zitadel's shape), as Grafana's
  `groups_attribute_path` does;
- for GitHub over `login`, `email`, `orgs` (the logins of organisations
  the user is an active member of) and `teams` (`org/slug`), which an
  OAuth App's `read:org` scope or a GitHub App's `members: read`
  permission covers.

An empty result falls through. For OIDC it falls to `defaultRole`,
which is `none` unless set: a person the mapping does not place is
refused, so a wrong claim name, a mistyped role or an IdP that admits
too widely fails closed, as Grafana's `role_attribute_strict` does.
`member` there gives everyone the IdP admits read access, as Grafana's
`auto_assign_org_role` gives a viewer, and is the one-line choice for an
instance whose IdP is the whole gate. For GitHub it falls to the
forge-derived membership that exists
today, now per account: a user who is an active member of `org-2` is a
member of `github/org-2` and of nothing else, a user whose own login is
a served account is a member of that account, and anyone else is
refused, so a GitHub sign-in never admits the whole of github.com. A
mapping that does not compile fails the file's load, like a bad filter;
one that errors at sign-in refuses that sign-in and logs why.

**An OIDC sign-in trusts the IdP for who gets in and the mapping for
what they may do.** An OIDC identity names no forge account, so an
instance-wide `member` from OIDC, whether the mapping said so or
`defaultRole` did, reads every account: the IdP's front door is the
gate, which is how a single-sign-on instance is meant to work. Anything
finer is the mapping's job, through the map form, and it is optional.
GitHub sign-ins keep asking the forge, because there the answer is free;
`auth.Resolve` keeps computing membership per account and stops
computing an admin role from organisation ownership. Where the forge and
the mapping both speak, the higher role wins, as Grafana takes the
higher of `org_mapping` and `role_attribute_path`.

Roles are computed at every sign-in and stored with the session, never
edited in the dashboard, so the file and the forge are the only places
roles come from; Grafana's `skip_org_role_sync` has no equivalent. A
consequence worth stating: being first grants nothing. There is no
"first user becomes admin" rule, so on a fresh instance with GitHub
sign-in, whoever signs in before the admin gets exactly what the
mapping and their organisation membership give them, which for a
stranger is a refusal, and the wizard is shown only to an admin. The
admin is whoever the file names, before the first sign-in happens.

**First run** is therefore: open the dashboard, sign in as the local
admin or through a provider whose mapping makes you an admin, and the
setup wizard of §2.6 takes it from there. There is no unauthenticated
setup state.

### 2.6 First run: a setup wizard over the same spec

A fresh instance has an empty spec. The first admin to sign in is met
by a setup wizard, a modal over the dashboard that walks the spec's
sections in the order they depend on each other. It is not a second way
to configure kritik: every step writes the instance spec through the same
API and validation as the settings pages, one audit event per step, so
closing the browser halfway loses nothing and reopening resumes at the
first incomplete step. An admin can dismiss it and use the settings
pages; a banner offers to resume until the instance can review, which is
one connection with at least one served account and a resolvable
`defaults.models.review`.

The steps:

1. **Listener.** Shows `KRITIK_WEB_URL`, the webhook path `/hooks`
   under it, and the connections the file declares, so what the
   deployment fixed is visible before anything depends on it. Nothing to
   write. The webhook listener and the dashboard share the one public
   origin: in the `all` topology one process serves both, and in the
   split topology the chart's single Ingress or HTTPRoute sends `/hooks`
   to the ingest Deployment and everything else to the web one (§2.1),
   so a deployment has one URL to get right.
2. **GitHub App.** Skipped when the file declares a connection.
   Otherwise §2.3's two ways, then the install link. The step completes
   when the `installation` webhook GitHub sends on install arrives, which
   the listener already parses, and it shows the account and repositories
   the delivery named; a delivery from an account other than the one the
   App was registered under is shown for the admin to accept or
   uninstall (§2.3), not served. When no delivery arrives the step waits
   on a manual "I installed it" instead, and polling covers reviews until a
   webhook exists.
3. **Model provider.** Type, optional `baseUrl`, the key, and optional
   pricing: the provider form as it is, plus a "test" that makes one
   cheap read call with the key and reports the failure verbatim. Then
   `defaults.models.review` and `fallback`, chosen from what the tested
   provider lists where it lists models.
4. **Embeddings.** The embedder that builds the similar-code index:
   endpoint, model, dimension and key, with the same test as a provider.
   Optional: left empty, reviews run without vector retrieval, as today
   with no `KRITIK_EMBED_*`. The embedder moves from the environment to
   the spec (`embedding`, one per instance) because it is a provider
   like any other to set up; what made it environment-only, that its
   dimension shapes the `index_chunks` table, becomes a confirmation:
   changing the model or dimension later is refused in the form unless
   the admin confirms a full reindex of every repository, which is what
   `KRITIK_REINDEX_ON_MODEL_CHANGE` guards today.
5. **Repositories.** The repositories each connection reaches, file or
   dashboard, read with its installation token from
   `GET /installation/repositories`, which is paginated
   ([list repositories accessible to the app installation](https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation)).
   They are grouped by account, and each can be left on (the default:
   everything the App reaches is reviewed, as today), turned off, or
   given a `mode`; the choices are written as the account's
   `repositories[]` entries only where they differ from the defaults, and
   the step also offers each account's `limits` and, optionally, its own
   provider key (§2.4), since a spend cap and whose key pays are the
   first things an admin serving someone else's organisation wants to
   settle. Finishing the step queues the onboarding index for the
   repositories left on, through the same leader path that onboards a
   repository learned from a webhook.
6. **Done.** A summary, and links to the connection's webhook
   deliveries and the queue page so the first review can be watched.

Step 3 is the instance's own keys, the default every account runs on;
step 5 is where an account's own key is entered on its behalf.

What the wizard needs that does not exist: `forge.Client` gains
`ListRepositories`, one arm for GitHub; the API gains a provider test
endpoint and a repositories-for-connection endpoint; the installation
webhook, parsed today and then only used to disable removed repositories,
also records the accounts and repositories it named so step 2 can show
them; the embedder's settings move into the spec with a reindex
confirmation on change; and the web app gains the modal, which composes
the connection, provider and repository field components that exist.

### 2.7 What is removed, by package

| Area                    | Removed                                                                                                                                                                                                                                                                                                                                                | Kept                                                                                                                                                                                                                                                                                                                 |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/forge`        | `forgejo/`, `gitlab/`                                                                                                                                                                                                                                                                                                                                  | `forge.go` (the interface), `github/`; gains `ListRepositories`                                                                                                                                                                                                                                                      |
| `internal/webhook`      | `parseForgejo`, `parseGitLab`, GitLab types, the Forgejo `is_pull` fallback, `X-Gitea-Signature` and GitLab token checks                                                                                                                                                                                                                               | the GitHub parsers, `unwrapFormPayload` (GitHub also sends form-encoded payloads), `Parse`'s switch                                                                                                                                                                                                                  |
| `internal/auth`         | `forgejo.go`, invites, `email:` operators, `IsOperator` against `web.operators`, `providers` keyed by sign-in name                                                                                                                                                                                                                                     | `forge.go`, `github.go`, `oidc.go`, `membership.go` (member per account; no admin from organisation ownership), sessions, middleware; gains `local.go` and `rolemap.go`                                                                                                                                              |
| `internal/configfile`   | `Tenants`, `Providers`, `Defaults`, `Polling`, `Indexing`, `Tools`, `Retention`, `Egress` and `Limits` from the file's schema (they move to the spec), `web.go`, tenant-level `Merge` and `SkippedTenant`, `ForgeGitLab`/`ForgeForgejo`/`ForgeGitea`, `Installation.Host`/`Token`/`GitToken`/`WebhookSecret`, `GitLabHost`, `GitLabSigningTokenPrefix` | types, validation, resolution, `Current`, `load.go`, `watch.go`, `env`/`file`/`sealed` refs, `Origin`; gains `auth.go` and a connection-level merge                                                                                                                                                                  |
| `internal/config`       | `KRITIK_EMBED_*` and `KRITIK_REINDEX_ON_MODEL_CHANGE` (the embedder moves to the spec)                                                                                                                                                                                                                                                                 | everything else; gains the `KRITIK_AUTH_*` / `KRITIK_CONNECTIONS_*` overlay onto the file (§2.2)                                                                                                                                                                                                                     |
| `internal/configsource` | tenant merge errors                                                                                                                                                                                                                                                                                                                                    | file watching, drift, load of the spec from Postgres, `NOTIFY` reload                                                                                                                                                                                                                                                |
| `internal/store`        | `dashboard_tenants`, `invites`, `memberships` (the role lives on the session), migration `0013_gitea`                                                                                                                                                                                                                                                  | everything else, `configsync`'s origin handling at connection level; migrations squashed into a new `0001_init.sql`, with `tenants` and `tenant_id` renamed `accounts` and `account_id` and the sign-in `accounts` table renamed `users`, since there is no release to upgrade from                                  |
| `internal/webapi`       | `policy.go`, `members.go`, `/tenants/{slug}` routes (re-keyed as `/accounts/{forge}/{name}`), `dashboardForgeHosts`/`dashboardProviderHosts` checks                                                                                                                                                                                                    | `manage.go`, `secrets.go`, `audit.go`, reads, actions, SSE; gains the provider test and connection repositories endpoints                                                                                                                                                                                            |
| `internal/web`          | tenant creation and slugs (pages become per-account), `MembersSection`, Forgejo and Gitea sign-in buttons                                                                                                                                                                                                                                              | admin editor and field components, review pages, usage, queue, admin console (listing the file's `auth` and connections read-only); gains the password form and the setup wizard                                                                                                                                     |
| `cmd/kritik`            |                                                                                                                                                                                                                                                                                                                                                        | roles; `web` always on in `all`                                                                                                                                                                                                                                                                                      |
| `charts/kritik`         | the file's `providers`, `defaults` and `tenants` examples, the `embedding.*` values, the second Ingress and HTTPRoute                                                                                                                                                                                                                                  | `config.file` (now optional), `existingConfigMap`, `reloadInterval`, `secretMounts`, `web.url` (required), `dashboard.keySecret`, the rest; gains an `auth:` block rendered to `KRITIK_AUTH_*` variables, with secrets from existing Secrets; one Ingress or HTTPRoute routes `/hooks` to ingest and the rest to web |
| `docs`                  | `connecting-a-forge.md`'s GitLab, Forgejo and Gitea sections; `dashboard.md`'s `web:` reference, roles, invites and provider-keys sections                                                                                                                                                                                                             | replaced by a setup guide (deploy with sign-in, open the dashboard, the wizard) and a file reference for `auth` and `connections`, mappings included                                                                                                                                                                 |

Deleting rather than keeping the GitLab and Forgejo clients dormant is
deliberate: dormant code still has to compile, test and be reasoned about
at every change to `forge.Client`, and git history keeps it for whoever
adds a forge back.

### 2.8 Two deployments this must fit

**A personal instance** serving `github/user-1` and
`github/org-1`, opened with a password and moved to GitHub
sign-in once it works.

- Day one: the chart with `KRITIK_AUTH_ADMIN_PASSWORD` and no file. The
  wizard registers a private App under `user-1` and a second under
  `org-1`, or one public App under either with both accounts
  listed; either way `accounts` names exactly those two. A provider key
  and the repositories follow.
- Later: a file with `auth.github`, using the client secret the manifest
  step showed once, or a fresh one generated in the App's settings on
  GitHub, and `roleMapping: 'login == "user-1" ? "admin" : ""'`. The
  file reloads without a restart, and the sign-in page shows the password
  form and the GitHub button side by side until the password is dropped
  from the chart values at the next rollout, or kept as break-glass.
  Without the mapping a GitHub sign-in as `user-1` would be a member of
  both accounts, by own login and by organisation membership, but not
  admin; the mapping is the one line that grants that.
- Or GitHub from the start instead, with no password: register the App
  by hand on GitHub first, with `<KRITIK_WEB_URL>/auth/callback/github`
  as its callback URL, and deploy with a file declaring it under
  `connections` and its client ID and secret under `auth.github` with
  the mapping above. The wizard then opens at the provider step.

**A work instance** serving `github/org-2` and `github/org-3`,
signed in through Zitadel.

- A public App owned by one organisation and installed on both, or a
  private App under each; `accounts` lists the two, and a stray install
  of a public App is shown and removable (§2.3).
- `auth.oidc` against Zitadel, with the roles scope in `scopes` and
  `rolesClaim: urn:zitadel:iam:org:project:<project id>:roles`, the claim
  Zitadel fills with a map of role name to organisation
  ([retrieve user roles](https://zitadel.com/docs/guides/integrate/retrieve-user-roles)).
  With Zitadel project roles `kritik-admin` and `kritik-user`:

  ```yaml
  auth:
    oidc:
      name: Zitadel
      issuer: https://sso.example.com
      clientId: "123456789"
      clientSecret: { env: ZITADEL_CLIENT_SECRET }
      scopes: [openid, profile, email, "urn:zitadel:iam:org:projects:roles"]
      rolesClaim: "urn:zitadel:iam:org:project:987654321:roles"
      defaultRole: none
      roleMapping: |
        "kritik-admin" in roles ? "admin"
          : ("kritik-user" in roles ? "member" : "")
  ```

  Everyone Zitadel gives `kritik-user` reads both organisations'
  reviews, `kritik-admin` administers the instance, and anyone Zitadel
  admits without either role is refused, since `defaultRole` is `none`.
  Nothing else gates access: Zitadel decides who is staff, the mapping
  decides what staff may do. Reading one organisation and not the other
  would be a map entry, and it is not needed here.

- org-2 brings its own Anthropic key under `accounts[].providers`,
  entered by the admin on org-2's behalf; org-3 runs on the instance's
  key; each has its own limits and usage page.

**The files, in full.** Everything below the `auth` and `connections`
keys is the whole file; providers, defaults, accounts and repositories
never appear in it.

The personal instance on day one has no file. Its deploy-time
configuration is two wiring variables and one secret:

```sh
KRITIK_WEB_URL=https://kritik.example.com
KRITIK_DASHBOARD_KEY=...            # openssl rand -base64 32, from a Secret
KRITIK_AUTH_ADMIN_PASSWORD=...      # from a Secret
```

The same instance after the switch to GitHub sign-in, with the App the
wizard registered still living in the dashboard, so `connections` stays
absent, and the password kept as break-glass:

```yaml
auth:
  admin:
    password: { env: ADMIN_PASSWORD }
  github:
    clientId: Iv1.abc123
    clientSecret: { env: GITHUB_CLIENT_SECRET }
    roleMapping: 'login == "user-1" ? "admin" : ""'
```

or, with no file at all, the same three settings as variables:

```sh
KRITIK_AUTH_ADMIN_PASSWORD=...
KRITIK_AUTH_GITHUB_CLIENT_ID=Iv1.abc123
KRITIK_AUTH_GITHUB_CLIENT_SECRET=...
KRITIK_AUTH_GITHUB_ROLE_MAPPING='login == "user-1" ? "admin" : ""'
```

The personal instance with an App the admin registered by hand and
keeps in git, public and installed on both accounts, doubling as the
sign-in client:

```yaml
auth:
  github:
    clientId: Iv1.abc123
    clientSecret: { env: GITHUB_CLIENT_SECRET }
    roleMapping: 'login == "user-1" ? "admin" : ""'
connections:
  - name: github
    forge: github
    accounts: [user-1, org-1]
    app:
      clientId: Iv1.abc123
      privateKey: { file: /var/run/secrets/kritik/github/private-key.pem }
      webhookSecret: { env: GITHUB_WEBHOOK_SECRET }
```

The work instance, with Zitadel and one private App per organisation,
each owned by the organisation it reads:

```yaml
auth:
  sessionTTL: 12h
  oidc:
    name: Zitadel
    issuer: https://sso.example.com
    clientId: "123456789"
    clientSecret: { env: ZITADEL_CLIENT_SECRET }
    scopes: [openid, profile, email, "urn:zitadel:iam:org:projects:roles"]
    rolesClaim: "urn:zitadel:iam:org:project:987654321:roles"
    defaultRole: none
    roleMapping: |
      "kritik-admin" in roles ? "admin"
        : ("kritik-user" in roles ? "member" : "")
connections:
  - name: org-2
    forge: github
    accounts: [org-2]
    app:
      clientId: Iv1.org2
      privateKey: { file: /var/run/secrets/kritik/org-2/private-key.pem }
      webhookSecret: { file: /var/run/secrets/kritik/org-2/webhook-secret }
  - name: org-3
    forge: github
    accounts: [org-3]
    app:
      clientId: Iv1.org3
      privateKey: { file: /var/run/secrets/kritik/org-3/private-key.pem }
      webhookSecret: { file: /var/run/secrets/kritik/org-3/webhook-secret }
```

Two connections is why this one is a file: the environment declares at
most one (§2.2). Had the admin created both Apps in the wizard
instead, the file would be the `auth` block alone.

## 3. Consequences

- Setup with nothing in hand is: install the chart with a database, one
  URL, a sealing key and an admin password; open the dashboard; follow
  the wizard through the App, a provider key and the repositories. Setup
  with an App and an IdP in hand is the same chart plus a file that
  declares both, and the wizard starts at the provider step.
- kritik gains the `KRITIK_AUTH_*` overlay, the file's
  `auth` block, an `instance_config` row, a local sign-in, CEL role
  mappings, the manifest callback, the setup wizard and the two endpoints
  and one forge method it needs.
- Roles are a function of the file and the identity provider, so who
  operates the instance is reviewable in git, as it was with
  `web.operators`; the dashboard cannot widen it.
- Serving another organisation's repositories from a personal instance
  is a supported shape, not a hosted mode: `org-2` gets its own
  limits, its own usage page and read access for its own members, and
  none of that needs a claim flow, a tenant admin or a key of its own.
- An App declared in the file is managed in git and can be rotated by
  rotating its Secret, as today; one created in the dashboard depends on
  the sealing key. Both shapes stay valid indefinitely, and an admin
  can move an App from one to the other by re-declaring it.
- The wizard and the settings pages cannot drift, because the wizard has
  no state of its own: "setup is complete" is a predicate over the spec,
  and each step is a settings form shown in sequence.
- The `forge.Client` interface, the `Forge` type and the per-forge switches
  are the extension points for a later forge. Adding GitLab back is a
  client, a parser arm, a connection credential shape and a UI form,
  and nothing in configuration or storage has to change.
- ADR-0003 loses its Forgejo section but keeps agentic reviews,
  `.kritik.yaml` and the templated contract, which are forge-independent.
- Roughly a third of the Go tests go with the code they cover: the forge
  clients, the tenant merge and collision tests, the policy table, the
  invite and claim flows.

## 4. Rejected alternatives

- **Environment only, no file.** Every deploy-time key does have a
  variable (§2.2), but a second App, or a private key someone would
  rather keep in a mounted Secret than a variable, wants a document, and
  the loading, reference resolution and reload code for one exists. The
  file stays, optional, under the variables.
- **File only, with secrets by reference as the sole environment form.**
  It keeps every setting to one owner, which ADR-0010 preferred, but it
  makes a values-driven deployment write a document to set five strings,
  and it is not how Grafana, which this deployment is modelled on,
  behaves. A stated precedence, environment over file, per key, is a
  small rule and one admins already know.
- **The file keeps the whole configuration and the dashboard only edits
  it.** Two writers of one document, which is what ADR-0010 ruled out;
  and the admin would still be writing providers, defaults and
  repositories in YAML, which is what the dashboard is for.
- **Editing a file-declared connection from the dashboard.** Same
  reason: the file would be overwritten or overruled, and git would stop
  describing what runs.
- **An unauthenticated setup page on a fresh instance**, with admins
  listed in the environment. It has a window in which anyone who finds
  the URL first can register an App, and a password form closes it for
  less code.
- **Connecting a GitHub account to an OIDC sign-in**, so that OIDC
  members see only the organisations GitHub says they belong to. It
  needs an identity-linking flow, a GitHub client on an instance nobody
  signs in to with GitHub, and a second redirect on every sign-in, to
  answer a question the IdP already answered by admitting the person.
  A map entry in the mapping covers the rare instance that wants
  per-organisation reads under single sign-on.
- **Storing roles in the dashboard** and letting an admin promote
  accounts there. It would make the file and the database two writers of
  one setting.
- **JMESPath for the mapping**, to match Grafana exactly. A second
  expression engine next to CEL, for expressions an admin writes once.
- **A wizard with its own state**, such as a `setup_progress` row or a
  draft spec applied at the end. The instance could then be half set up
  in two places at once, and an admin who left the wizard for the
  settings pages would find them disagreeing. Writing each step through
  is what makes the wizard safe to abandon.
- **Keep the other forge clients behind the greyed-out picker.** See §2.7.
- **Serving whoever installs a public App.** ADR-0013 rejected it for a
  host, and it is worse for a self-hosted instance: the admin would pay
  for a stranger's reviews on the admin's runners. An explicit
  `accounts` list is one line and closes it.
- **Exactly one App per instance.** A list is already what exists, costs
  nothing, and is what a second forge will need.
- **An account admin role**, derived from GitHub organisation ownership
  and allowed to enter its organisation's own key and pick its models.
  An earlier draft had it. It is a third role, a policy table and a
  per-account write path kept alive for one field, on an instance whose
  admin already talks to that organisation; the key is handed over out
  of band instead, and the role can come back if that proves too manual.
- **One instance-wide tenant**, with accounts as a flat list. It was an
  earlier draft of this ADR. It leaves `tenant_id` and row-level security
  in the schema with nothing to isolate, drops the middle scope of
  ADR-0010's hierarchy, and lets a member of one served organisation
  read another's reviews. An account is the unit every existing row,
  policy and setting already wants.

## 5. Deferred

- **Export and import of the dashboard spec**, as YAML with secrets
  left as `{ sealed: ... }` or stripped, for backup and for moving an
  instance: the advanced JSON view shows the spec but cannot restore it.
  Wanted before the first release; not part of this ADR's delivery.
- **A password hash in the environment** instead of the password, and
  changing the admin password from the dashboard.
- **An editor role** between admin and member, allowed to re-run,
  cancel and reindex but not to change configuration, if members turn out
  to need it.
- **Importing a file connection into the dashboard** with one click,
  instead of re-entering it, once there is a reason to prefer sealed
  storage over a Secret.
- **GitHub Enterprise Server.** The connection `host` field returns
  when it does; the picker already shows the option disabled.
- **GitLab, Forgejo and Gitea**, through the extension points of §3.

## 6. Delivery

Stacked pull requests, each leaving `main` green:

1. This ADR; ADR-0013 marked withdrawn; the amended sections of ADR-0003,
   ADR-0009, ADR-0010 and ADR-0012 annotated.
2. Remove GitLab, Forgejo, Gitea and GitHub Enterprise Server: clients,
   parsers, sign-ins, configuration fields, migration, chart example and
   docs. The picker shows them disabled.
3. Sign-in: the file's `auth` block replaces `web`, with CEL role
   mappings and the two roles; the local admin; the environment
   overlay onto the file, with the `connections` variables;
   `web.operators`, invites and the memberships table go, with per-account
   roles held on the session. The file still carries everything else at
   this step, so the change is testable on its own.
4. Instance configuration in Postgres: `instance_config` replaces
   `dashboard_tenants` and the file's spec-owned keys; the file keeps
   `auth` and `connections`; tenants become accounts, in the schema too,
   and the sign-in `accounts` table becomes `users`;
   the embedder moves into the spec; the merge moves to connection
   level; migrations squashed.
5. GitHub App setup: the manifest flow and the manual form at instance
   scope, the private-or-public choice, the shown-once client secret.
6. The setup wizard: `ListRepositories`, the provider test and
   repositories endpoints, the installation webhook recording what it
   named, and the modal over the existing field components. With it,
   settings sections and fields as command-palette entries, and a filter
   over a repository's effective settings, since every setting now lives
   in the dashboard.
7. Chart and documentation: `web` always on, `web.url` required, one
   Ingress or HTTPRoute routing `/hooks`, the
   file optional and reduced to `auth` and `connections`, README, the
   setup guide and the file reference replacing `connecting-a-forge.md`
   and the `web:` half of `dashboard.md`.
