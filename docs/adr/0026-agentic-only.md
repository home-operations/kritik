# ADR-0026: every review is agentic, and the index reaches it through the gateway

- **Status:** Proposed
- **Date:** 2026-09-30
- **Authors:** onedr0p.
- **Amends:** [ADR-0003](0003-forgejo-agentic-review.md) §2.3 (the
  `mode` key), §2.6 (agentic mode as one of two) and §4 (similar-code
  context in agentic mode, deferred), [ADR-0004](0004-model-gateway.md)
  §2 (`search_index` on the gateway, not built) and
  [ADR-0021](0021-configuration-shape.md) §2.1 and §2.3 (`mode` in the
  shape and in `.kritik.yaml`).

> Scope: how a review runs and what context it is given. The contract,
> publishing, follow-ups and how the index is built are unchanged.

## 1. Context

A review runs one of two ways. In `single` mode the runner writes a
context pack and the worker makes one structured model call over it,
adding stage 4 on the way: the diff's hunks embedded and the nearest
chunks of the repository's index. In `agentic` mode the runner writes the
same pack and runs a tool loop over it through the gateway, with stages 1
to 3 and no stage 4, since the runner holds neither the embedder's key
nor read access to the index (ADR-0003 §4; ADR-0004 lists `search_index`
on the gateway as not built). Agentic has been the default since
2026-09-29.

Two modes cost more than a second code path. Admission and caps, model
leases, when a rule's `whenExpr` is judged, and publishing each branch on
the mode; there are two system prompts; `mode` is a key at every layer of
the configuration and in `.kritik.yaml`; and the gateway is optional, so
the chart carries a way to run without it. The index, which every
instance with an embedder builds and keeps current on every push, feeds
only single mode: an instance on the default pays for the embeddings and
uses none of them.

What single mode offers is one model call, cheaper and faster than a
loop. The loop already makes that call when `agent.maxSteps` is 1: its
last step forces `submit_review`, and its first prompt carries the same
diff and context. The two ask the same of a model, since single mode's
findings also come back as a forced call to a named tool.

## 2. Decision

### 2.1 One mode

Every review runs in the runner's agent loop, through the gateway. `mode`
leaves the configuration, `.kritik.yaml`, the runner's spec, the API and
the dashboard.

The cheap review is `agent.maxSteps: 1`: one forced `submit_review` call
over the prompt a single-mode review had. It is an operator setting, at
`defaults`, `owner/*` or `owner/name`; a repository's own file no longer
chooses how its review runs, as it already does not choose agent limits
(ADR-0021 §2.3). The account's `limits` bound what any review costs, as
before.

The bench keeps measuring one structured call over the service's own
prompt, now the agentic system prompt, which is what `agent.maxSteps: 1`
sends.

### 2.2 Stage 4 through the gateway

The gateway serves `POST /v1/similar`, authenticated by the run's token
like its model endpoint. The request has no body: the gateway reads the
run's own context pack, which the runner writes before its agent starts,
and returns what single mode's stage 4 did, unchanged: up to 12 of the
diff's hunks embedded with the instance's embedder, and the 10 nearest
chunks of the repository's active index generation, outside the changed
paths and above a similarity of 0.5.

The embedding is reserved and charged against the run's token budget
like a step, and recorded as usage where the caps count it, so a runner
that asks twice pays twice from a budget it cannot raise. It takes the
account's embedding slot as single mode did.

The runner asks once, before composing its prompt, and adds the chunks
as stage 4. Stage 4 stays best effort: an instance without an embedder,
a repository without a completed index, or a refused or failed request
leaves it out and the review goes on.

The runner still holds no embedder key and cannot read the index. What
reaches it is chunks of the repository it has already checked out.

### 2.3 The gateway is required

`kritik serve` always runs the gateway (ADR-0024), and every review now
needs it, so the chart's `gateway.enabled`, which could turn it off, goes.

### 2.4 As built (2026-09-30)

Where the build differs from §2.2:

- **The request carries its queries.** `POST /v1/similar` takes a body,
  `{"queries": [...], "exclude": [...]}`: up to 12 texts of 3,000
  characters, and the paths to leave out of the answer. The gateway
  embeds the texts and searches the run's repository; it no longer reads
  the run's context pack, so the runner can ask before it writes the pack,
  and the agent can ask with a query of its own. The answer says whether
  the repository has an index at all (`indexed`), so a run tells an empty
  answer from no index. The reservation is the queries' characters at four
  a token.
- **`search_code` is an agent tool.** A repository with an index gives the
  agent `search_code(query)`, the same route with the agent's text, at
  most ten calls a run, and the system prompt says the index is of the
  default branch and may lag the head. §4's "left until reviews show the
  agent wanting it" is settled the other way: without the tool, nothing
  could show it.
- **The pack records the review's decisions, stage 4 included.** The
  runner asks for similar code, builds the prompt and decides everything
  the worker reads back before it writes the pack: whether the review is
  skipped (`skip_reason`: only ignored paths changed, or a bot's patch
  unchanged), what it builds on (`scope`, `scope_reason`), which rules the
  prompt was given (`rule_ids`), and the notes the summary states, the
  files it could not read and the prompt's cuts among them. The stages
  include the similar chunks, so the dashboard and follow-ups see them.
  The worker no longer repeats the skip, scope and rule decisions after
  the run, and a skipped review writes no agent run.
- **`kritik_context_chunks_total`** counts, by stage, the chunks each
  review's prompt was given, so whether stage 4 reaches prompts is
  measurable.
- **The bench sends one agent step**: the agentic system prompt, the
  read-only tools and one forced `submit_review`, what `agent.maxSteps: 1`
  sends, less stage 4, which needs an index.

## 3. Consequences

- **One review path.** Admission, leases, caps, rule judgement and
  publishing lose their mode branches, and the worker no longer calls a
  model for a review, only for follow-ups.
- **Agentic reviews get similar-code context**, and an instance with an
  embedder uses the index it pays for.
- **A repository cannot ask for a cheaper review** in its own file; an
  operator sets `agent.maxSteps` for it instead.
- **Every review runs a runner Job** and reaches its model through the
  gateway, including the one-call review.
- **A fallback on another provider no longer applies to reviews.** Only
  the worker's own call switched providers; the gateway hands a provider
  the fallback only when it is on the same one, which covers OpenRouter's
  server-side fallback. Switching providers in the gateway stays deferred
  (ADR-0004 §2).
- **A `context` file is a pointer.** The agent is told where it is and
  reads it with its tools; no review has its content inlined any more.
- **`mode` is an unknown key.** A configuration that sets it, or
  `KRITIK_DEFAULTS_MODE`, fails to load, and a `.kritik.yaml` that sets it
  is ignored with a note, as for any unknown key.

## 4. Rejected alternatives

- **Keep `mode: single` as a name for `agent.maxSteps: 1`.** Two ways to
  write one setting, and the old name would promise the worker-side call
  that no longer exists.
- **Delete the index with single mode.** It would end the embedder, the
  index Jobs and the VectorChord requirement, but stage 4 is the context
  `grep` cannot find by name, and whether it earns its cost is for the
  bench to measure with the stage in place.
- **Similar code as an agent tool now.** A `search_similar(query)` on the
  same route lets the model ask mid-run; it is left until reviews show the
  agent wanting it.
