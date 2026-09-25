# ADR-0004: the worker is the model gateway; no provider key enters a runner

- **Status:** Proposed
- **Date:** 2026-09-25
- **Amends:** [ADR-0003](0003-forgejo-agentic-review.md) §2.6 (agentic
  mode) and §2.9 (the worker–runner protocol): the model key stops
  travelling in the run's Secret; the runner calls a gateway in the
  worker with a per-run token instead. Everything else in ADR-0003 stands.
- **Replaces:** the earlier ADR-0003 draft of this proposal (merged as
  `0003-agentic-review-loop.md` on 2026-09-25, removed by this ADR). Its
  loop, tools, trajectory record and rollout gate were built by ADR-0003
  as merged; what remains of it is the credential question, decided here.
- **Amended by:** [ADR-0008](0008-runner-tools.md), which makes the
  gateway the runner pod's only route out, as a forward proxy with a host
  allowlist, before it fronts model calls.
- **Authors:** onedr0p.

> Scope: where the provider credential lives when a runner pod runs the
> model loop, how a runner reaches a model, how usage and caps are
> accounted for agentic runs, and how the change rolls out. It does not
> change the loop, the tools, the contract, `agent_runs`, or the job
> document beyond the fields named below.

## 1. Context

ADR-0003 as merged runs the review loop in the runner pod, over the head
commit's git objects with read-only tools, and hands the runner the model
key inside the run's Secret. It says why that is tolerable: the pod runs
no repository code, the tools are read-only, and the only output is
findings text the worker validates, so a prompt injection can at worst
shape that text. It also says what to do about the residual risk: give
each tenant its own key with a spending limit, and on Forgejo a read-only
git token.

That residual risk is real and lives outside kritik. A provider key in a
pod that parses untrusted repositories can be read by anyone who can read
the pod's Secret or exec into it, and a prompt injection that talks the
model into calling a tool with the key in its arguments would place it in
`agent_runs` and the log tail, which is why ADR-0003 masks secrets out of
stored logs. Spend on a leaked key is bounded by the provider's limit, not
by kritik's caps, and one key per tenant is a recommendation the operator
may not follow. The single-shot path never had this exposure: the worker
called the model and the runner only ever produced a context pack.

The worker already holds every provider credential, resolves tenants,
takes leases, checks caps and records usage. Placing it between the
runner and the provider restores the boundary ADR-0002 §2.3 drew, at the
cost of one listener.

## 2. Decision

### 2.1 The gateway

The worker (and `all`) serves a third listener, the `gateway` port
(`KRITIK_GATEWAY_ADDR`, default `:8082`), with one purpose: forward a
runner's model calls to the tenant's provider. The endpoint speaks the
OpenAI chat completions wire format, request and response, streaming
included, because that is what ADR-0003's `openai` and `openrouter`
adapters already speak and what the runner's `Stepper` can be pointed at
unchanged; an `anthropic` provider is reached through the same endpoint
by translating in the worker, where the Anthropic adapter lives.

A request carries `Authorization: Bearer <run token>`. The gateway
resolves the token (§2.2) to its run, tenant, review and repository,
refuses anything else, and then, under that tenant's context:

- checks the tenant's caps and the run's remaining token budget exactly
  as ADR-0003 §2.6 does today, refusing with `429` and a body the runner
  turns into a `capped` stop;
- takes the model lease if the run does not hold one already (ADR-0003
  keeps the lease for the Job's lifetime; the gateway reuses it);
- rewrites the request's `model` to the model the run was granted (the
  runner names `review` or `fallback`, never a provider model id, so it
  cannot pick a model the tenant did not configure);
- forwards to the provider with the tenant's credential and the
  attribution headers, passes the response through, and parses `usage`
  from it (the final chunk when streaming) into a `usage` row and the
  metrics, charging the run's budget;
- records the step in `agent_runs`' timeline as ADR-0003 does, so the
  runner's own record and the gateway's agree.

Tools that need tenant state, `search_index` first, are served on the
same port under the same token, so the runner's only network peers are
DNS, the git remote, Postgres and the gateway.

### 2.2 The run token

The worker mints a token per run when it creates the run row: 32 random
bytes, presented as `krk_<hex>`, stored only as its SHA-256 in a
`gateway_tokens` table with the run, tenant, review and repository ids
and an expiry (the Job deadline plus `KRITIK_GATEWAY_TOKEN_TTL`, default
one hour). The table has no row-level security on purpose: it is looked
up by the token's hash before any tenant is known, holding the token is
the authorisation, and a row reveals only the ids of the run it belongs
to. The token is revoked when the run finishes and swept with expired
tokens on every revocation.

The token travels to the pod the way ADR-0003 §2.9 already delivers the
git token and the job document: a key of the run's Secret. The job
document gains `model.gatewayUrl` (`KRITIK_GATEWAY_URL`, the in-cluster
address of the worker's gateway Service) and loses the provider key and
base URL; `model.provider`, the model names and pricing stay so the
runner can keep its own usage estimate for the stop decision. A runner
whose document names no gateway runs single shot as before.

The trust boundary reads, once more: **no long-lived secret and no
provider key enters a runner pod.** The pod holds its git token, its
database role, and a gateway token that is scoped to one run, expires
with it, and can spend only what that run's budget allows.

### 2.3 What stays and what moves

Stays in the runner: the loop, the tools, the step and byte caps, the
`agent_runs` row and its timeline, the heartbeat, the job document. Moves
to the worker: the provider credential, the base URL, cost computation
from pricing, and the authoritative usage row per step. The runner's
usage estimate becomes advisory; the gateway's count is what the caps
see.

### 2.4 Chart and network

The chart adds a `gateway` container port on `all` and `worker` pods, a
`<release>-gateway` Service selecting them, and the gateway port to the
worker's NetworkPolicy ingress and the runner's egress. `KRITIK_GATEWAY_URL`
defaults to that Service's cluster address. A split topology therefore
has a fourth Service; the `ingest` role does not serve the gateway.

### 2.5 Rollout

1. Gateway listener, `gateway_tokens` and the token in the run's Secret,
   with the job document still carrying the key: the runner is unchanged
   and the gateway is inert.
2. The runner's `Stepper` pointed at the gateway when the document names
   one, the key removed from the document, the chart's Service and
   policies, and the integration test for agentic mode rerun through the
   gateway.
3. `KRITIK_GATEWAY_URL` becomes required for agentic mode; a document
   without it is refused at load, so no deployment can fall back to a key
   in the pod by omission.

### 2.6 As built (2026-09-25)

The three rollout steps landed as one change, since kritik has no release
a key in the pod would have to stay compatible with. Where the build
differs from the text above:

- **Every provider through the worker's adapter.** The endpoint decodes a
  chat completions request into kritik's own step request and answers it
  through the tenant's provider adapter, the one single mode uses, then
  encodes the result as a chat completion, with the cost and serving
  provider in `usage` the way OpenRouter reports them. `openai` and
  `openrouter` are not passed through: one path for every provider, and
  the adapters already compute usage and cost. Streaming is not served;
  the runner's adapter does not ask for it.
- **The job document is version 2.** Its model block is the gateway URL
  and the name `review`; the provider, fallbacks and pricing are gone with
  the key, because the gateway reports each step's cost and applies the
  same-provider fallback itself. A document without a gateway is refused,
  so an agentic runner never runs single shot, and the worker refuses an
  agentic review when `KRITIK_GATEWAY_URL` is empty.
- **`gateway_tokens` carries the grant.** Beside the ids and the expiry, a
  row holds the model and fallback references the run may call and its
  budget and spend, so a step is checked against the run's own grant, not
  the configuration as it stands when the step arrives.
- **Leases and timelines stay where they were.** The worker holds the run's
  model lease for the Job's lifetime, so the gateway takes none. The
  gateway's record of a step is its usage row, not an `agent_runs` timeline
  entry, since that row is the runner's to write once, at the end; the two
  agree to the token (checked live on a five-step review).
- **A budget refusal ends the run as `budget`.** The gateway answers `429`
  with code `budget_exhausted` and `X-Should-Retry: false`; the runner's
  loop stops as its own budget check would, and the review ends incomplete.
  Every other refusal also carries `X-Should-Retry: false`, since the
  worker's adapter has already retried the provider, and a provider error
  reaches the runner with the key masked out of it.
- **`NO_PROXY` names the gateway.** Runner pods send everything through the
  forward proxy on the same port, so the gateway's own host is excepted,
  or a model call would arrive as a proxy request for a host the allowlist
  does not name.
- **Not built:** `search_index` on the gateway, and a fallback model on
  another provider, which the gateway now makes possible (ADR-0003 §4).

## 3. Consequences

**Positive.** No provider credential in any pod that touches repository
content; spend is bounded by kritik's caps rather than the provider's;
usage and cost are recorded where leases and caps live, in one place for
both modes; the runner needs no provider-specific code; a leaked run
token is worthless after the run.

**Negative.** One more listener and Service, and one more hop per model
step. Streaming through a proxy is more code than a direct call. The
worker becomes a dependency of a running review: a worker restart ends
the runs it was serving, which the heartbeat and supersede rules of
ADR-0003 §2.9 already handle for the worker's own death.

**Unchanged.** The loop, tools and their caps; the contract; the job
document's shape apart from the model block; the leader's Secret sweep;
the single-shot path.

## 4. Alternatives considered

- **Keep the key in the pod, as ADR-0003 merged it.** Simplest and
  already built. Rejected as the end state because the mitigation is an
  operator recommendation rather than a property of the system; kept as
  the interim state through rollout step 1.
- **A budgeted virtual key from an external proxy (LiteLLM).** Moves the
  problem to another component and puts per-tenant budgets outside
  kritik's leases and usage rows. A deployment that runs LiteLLM can still
  point the worker's provider at it; the runner only ever sees the
  gateway.
- **Loop in the worker with tools served by the runner.** Discussed in
  the replaced draft; rejected there for the round trip per tool call and
  for putting the agent beside write-back, and moot now that ADR-0003's
  loop in the runner is built.
