# ADR-0013: a hosted instance serves whoever installs its App and brings a model key

- **Status:** Withdrawn
- **Date:** 2026-09-27
- **Superseded by:** [ADR-0014](0014-github-app-only-self-hosted.md):
  kritik is a self-hosted instance and is not run as a service for others.
  Its per-tenant provider keys (§2.4) survive there as per-account keys.
- **Authors:** onedr0p.

> Scope: running kritik as a service for others. The host owns one public
> GitHub App. Anyone installs it, claims their account in the dashboard,
> and brings their own model key. This ADR covers how deliveries reach the
> right tenant, how an account is claimed, where the customer's key lives,
> and what the host still pays for. It does not cover billing, GitHub
> Marketplace, or GitLab.

## 1. Context

kritik is self-hosted today. The operator writes, or creates from the
operator console, every tenant: only an operator creates a dashboard
tenant ([dashboard](../dashboard.md)).

- **Model keys.** Providers and their keys live in the configuration file
  alone. A dashboard tenant may only reference the providers and models
  the file declares (ADR-0009), and per-tenant providers are deferred
  (ADR-0010 §5).
- **Accounts.** An installation serves the accounts it lists and ignores
  deliveries for any other. Its GitHub App's installation, and so the
  token, is found from each repository's owner.

A hosted instance inverts who does what. The host registers one public App
and sets its one webhook URL. Customers install it on their own
organizations and pay for their own model use. Four things are missing:

1. **Routing.** All of the shared App's deliveries arrive at one URL, and
   each has to reach the tenant that owns its account. The hook path names
   one installation today.
2. **Claiming.** An account has to be tied to a tenant by someone entitled
   to it, and that person is not the operator.
3. **Model keys.** Each tenant needs its own provider credentials.
4. **Costs.** The host still pays for runner compute, the embedding
   index, and storage.

What exists already carries much of it:

- row-level security per tenant;
- sandboxed runners;
- the model gateway, which forwards "with the tenant's credential" and
  never lets a provider key into a runner pod (ADR-0004);
- sealed dashboard secrets;
- dashboard membership derived from forge accounts.

## 2. Decision

### 2.1 Hosted mode is off unless the file configures the shared App

A `hosted` block in the file holds the shared App's client ID, private key
and webhook secret, and turns the mode on. Without it, nothing below
exists and an instance behaves as it does today.

### 2.2 The shared App's deliveries are routed by account

The shared App's webhook points at one hook path of its own. The listener
verifies a delivery with the shared App's secret, then finds the tenant
installation that serves the delivery's account. That installation is a
dashboard installation marked as using the shared App, whose `accounts`
are its claims.

A delivery for an account nobody has claimed is accepted and ignored, as
an undeclared account is today. So installing the App grants nothing by
itself: no reviews, no runner, no model call.

### 2.3 An account is claimed by someone who can administer it

After an install, GitHub sends the person to the App's setup URL with an
`installation_id`. GitHub's documentation warns that this parameter can be
spoofed and should be checked with the user's own token
([about the setup URL](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url)).

The person signs in with GitHub. kritik confirms, with their token, that
they can see that installation and that its account is either their own
login or an organization they are an admin of: the same organization role
sign-in already reads.

They then do one of two things:

- create a tenant, which is self-serve in hosted mode, optionally held for
  the operator's approval;
- add the account to a tenant they already administer.

An account belongs to at most one tenant. By default a claim makes one
tenant per account, so members of one organization never read another's
reviews.

### 2.4 A hosted tenant brings its own model key

Dashboard tenants gain `providers`, with the same shape as the file's.
Their keys are sealed at rest like installation secrets. A hosted
tenant's models resolve to its own providers, and the
gateway forwards its calls with that tenant's key. The key stays out of
runner pods, which is the reason the gateway exists.

A hosted tenant without a usable key is claimed but inactive. Its pull
requests are not reviewed, and the dashboard says why. The host may still
offer its own providers to chosen tenants, through the `allow` bounds a
tenant's models already obey.

### 2.5 What the host pays for is bounded per tenant

- **Compute.** Runner pods, the database and the webhook listener stay the
  host's. The per-tenant limits that exist (concurrency, reviews per day)
  take defaults from the `hosted` block for every hosted tenant.
- **Embeddings.** The similar-code index uses the instance's embedder,
  because the index table has one embedding dimension per deployment. A
  hosted tenant's indexing counts against its limits, and the `hosted`
  block can turn indexing off for hosted tenants.

### 2.6 The shared App's key is the host's most sensitive secret

It can read every installed organization's code. It lives where the file
keeps secrets, never in a dashboard spec. Losing it is losing every
customer's trust at once, so rotating it is part of operating the mode.

## 3. Consequences

- Anyone can start using a hosted instance by installing its App, signing
  in, claiming their account and entering a model key. The operator no
  longer creates each tenant, unless approval is kept on.
- Installing the App alone costs the host nothing. Every review, runner
  and model call belongs to a claimed tenant within its limits, on its own
  key.
- Self-hosted instances are untouched. Per-tenant Apps (ADR-0012) and
  file-configured installations keep working next to the shared App, so a
  customer can still bring an App of its own.
- kritik gains a second hook path, a claim flow, per-tenant providers and
  the `hosted` block. Of these, per-tenant providers are also useful
  without hosted mode.

## 4. Rejected alternatives

- **Serve every account that installs the App by default.** Anyone who
  installed it would get reviews at the host's expense, on the host's
  runners, with their code inside the host's cluster. With no claim,
  nothing would tie an account to a tenant either.
- **Only per-tenant Apps (ADR-0012).** Each customer would register an App
  before anything worked. That suits an organization running kritik for
  itself, not a service people try in a minute.
- **One tenant holding every claimed account.** Members of any
  organization would read every other organization's reviews.

## 5. Deferred

- **Billing and usage-based plans.** The usage table already records
  tokens and cost per tenant.
- **Embeddings on the tenant's own key.** It needs the tenant's embedder to
  match the deployment's model and dimension, or an index per dimension.
- **Forgejo and Gitea.** They have no Apps. A hosted tenant can already
  bring a bot token as a dashboard installation with its own hook path, so
  there is nothing to route.
- **GitLab**, until it has a client.
