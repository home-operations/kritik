# ADR-0015: instance defaults in the file, under the dashboard's

- **Status:** Superseded
- **Date:** 2026-09-29
- **Superseded by:** [ADR-0019](0019-configuration-in-git.md): the file
  holds the whole configuration, so its defaults are the defaults. Its
  environment variables stay.
- **Authors:** onedr0p.
- **Amends:** [ADR-0014](0014-github-app-only-self-hosted.md) §2.2 (what
  the file holds, and that there is no per-field overlay).

> Scope: where an instance's model providers, default review and fallback
> models, and embedder may be set. Nothing about how they are used
> changes.

## 1. Context

ADR-0014 §2.2 left the file `auth` and `connections` and moved everything
else to the instance spec, edited in the dashboard. A deployment that
already holds its model key as a Secret, such as an OpenRouter key an
admin keeps in a password manager, still has to paste it into the setup
wizard before the first review, and every rebuilt instance starts with no
model. Those settings are what a deploy most wants fixed from the start,
and what an admin most often wants to adjust later without a redeploy.

## 2. Decision

The file, and its environment, may also set `providers`, part of
`defaults` (`models.review`, `models.fallback`, `mode`,
`review.thoroughness`, `forks` and `settle`) and `embedding`. They are the
instance's defaults, under the spec's:

- a provider the spec declares by the same name replaces the file's
  whole, and the spec may add others;
- a default the spec sets replaces the file's of that key, and an account
  or repository entry overrides either as before;
- an embedder the spec sets replaces the file's whole.

The environment follows ADR-0014's rule for connections: it declares at
most one provider, `KRITIK_PROVIDERS_NAME` (default `openrouter`),
`_TYPE` (defaulting to the name when that is a provider type),
`_BASE_URL` and `_API_KEY[_FILE]`, which replaces the file's provider of
that name or joins them. `KRITIK_DEFAULTS_MODELS_REVIEW`, `_FALLBACK`,
`_MODE`, `_REVIEW_THOROUGHNESS`, `_FORKS` and `_SETTLE` set the defaults,
and `KRITIK_EMBEDDING_BASE_URL`,
`_API_KEY[_FILE]`, `_MODEL` and `_DIMS` the embedder key by key. A
variable under these prefixes that names no key fails startup.

The file's keys are references, `{ env: NAME }` or `{ file: path }`, as
every file secret is; the spec's stay sealed. The admin console lists the
file's values with their source and whether the spec overrides them, and
the dashboard's forms show them as what an empty field inherits.

An embedder change reaches the index as before: the leader rebuilds the
index whenever the running embedder's model or dimension differs from the
index's. A file edit that changes them rebuilds without asking, as a
deliberate change to what is deployed; a dashboard write that drops the
spec's embedder for a different one the file sets asks for the same
confirmation as any other.

## 3. Consequences

- An instance can review from its first start with nothing entered in the
  dashboard, and the setup wizard finds its model steps already done.
- The one-owner rule of ADR-0010 is relaxed for these three keys: the file
  owns a default, the spec an override. The admin console says which runs.
- A spec that removes a provider the file's default model names is
  refused like any other spec that would not run.

## 4. Rejected alternatives

- **The file wins over the spec**, as for connections. It would make these
  settings read-only in the dashboard, which is exactly where an admin
  adjusts a model.
- **One provider per variable name**, `KRITIK_PROVIDERS_<NAME>_*`. Names
  lose their case and hyphens in a variable, and a second provider is rare
  enough for the file.
