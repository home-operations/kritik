# ADR-0003: the review is an agent in the runner, the worker is its model gateway

- **Status:** Proposed
- **Date:** 2026-09-24
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.3 (trust
  boundary), §2.10 (context retrieval), §2.11 step 7 (review), §2.12
  (model adapters) and §2.14 (evaluation). Everything else in ADR-0002
  stands.
- **Authors:** onedr0p, after review feedback on ADR-0002 as built.

> Scope: where the model loop runs, where the model key lives, how a
> runner reaches a model and the tenant's index, what a review records,
> and how the change is measured before it becomes the default. It does
> not change tenancy, the store, the queue, the forge surface, the chart,
> or the index.

## 1. Context

ADR-0002 as built reviews a pull request in one model call. The runner
pod gathers context up front (the diff, the declarations it touches,
definitions of identifiers on changed lines, callers of changed
declarations) and the worker adds the nearest indexed chunks, builds one
prompt, and takes one structured answer. The model never asks for more.

The design bet was that deterministic retrieval recovers most of what an
agent would go and read, at lower cost and with a reproducible prompt.
Review feedback on the built system put it plainly: the review is single
shot, there is no loop, and the rule that no secret enters the runner pod
is what forecloses one. The checkout lives in the runner and the model
key lives in the worker, on opposite sides of a boundary the ADR drew on
purpose.

Prior art runs a loop. Kodus's finder agent reads and greps a sandboxed
checkout with a tool budget before writing findings (ADR-0001 §1.4), and
the coding agents the org already uses work the same way. For a
dependency bump the difference is nil; for "is this refactor safe" a
single shot with heuristic retrieval misses what an agent would open: the
caller two hops away, the test that pins the old behaviour, the config
that names the function. The evaluation harness (ADR-0002 §2.14) exists
to measure exactly this and has not yet been run with a model.

The question is not whether to add a loop but where to put it without
giving untrusted repository content a path to model spend or to another
tenant's data.

## 2. Decision

### 2.1 Where the loop runs

**The runner pod is the reviewer.** After the fetch and the context
stages it already performs, the runner runs the model loop itself: the
seed prompt is the context pack it built, the tools are functions over
its own checkout, and it stops when the model returns findings or a cap
is hit. It writes the findings and the whole trajectory (every step,
tool call and result, with token counts) under its run id, the same way
it writes the context pack today. The worker keeps everything else:
queue, staleness, leases, caps, write-back, index, poll.

Tools available in the runner, all read-only over the head and base
trees the runner holds:

- `read_file(path, start, end)`: a line range of the head version, or of
  the base version when asked.
- `list(path)`: entries of a directory.
- `grep(pattern, glob)`: fixed-string or regular-expression search over
  the head tree, bounded in matches and bytes.
- `symbols(path)`: the declarations the chunker sees in a file, with
  their line ranges.
- `blame(path, start, end)`: which commit last touched each line, when
  history is available; the runner fetches at depth one, so this is
  usually only "in this pull request or before it".
- `search_index(query)`: the nearest indexed chunks, served by the
  worker (§2.3), since the index and the embedder are not in the pod.

No tool executes repository code in v1 of the loop. Linters and tests
inside the sandbox are the natural next tool set and are possible only
because the loop runs where the checkout is; they are deferred until the
harness shows the read-only loop pays for itself.

**Caps per run**, enforced in the runner and echoed in the run row: steps
(default 12), tool calls, bytes returned by tools (default 256 KiB),
output tokens per step, and the Job's own deadline. Hitting a cap ends
the loop with whatever findings the last step produced and marks the
review `completed` with `capped_loop` in its record, never `failed`: a
partial review that says it stopped early beats none.

Single shot is the loop with zero steps allowed. It stays as the mode the
harness compares against and as the per-tenant setting for anyone who
wants cheap and deterministic reviews.

### 2.2 Where the key lives

**The worker is the model gateway.** It gains one listener, the
`gateway` port, serving an OpenAI-compatible chat completions endpoint
for runners. A runner authenticates with a per-run token; the worker
resolves the token to its run, tenant and review, checks the tenant's
lease and caps, swaps in the tenant's provider credential, forwards the
call to the configured provider through the same adapters ADR-0002 §2.12
describes, records usage and cost against the review, and returns the
response. Streaming is passed through. The runner uses Fantasy's
OpenAI-compatible provider pointed at the gateway with the run token as
its API key, so the runner has no provider-specific code at all.

The trust boundary of ADR-0002 §2.3 is restated: **no long-lived secret
and no provider key enters the runner pod.** The pod holds the git read
token and the runner database role it already holds, plus a gateway
token minted for that run, delivered the same way (the per-run Secret
the Job owns), scoped to that run's tenant, model and budget, and
revoked when the run row is finished or the Job is deleted. A
compromised runner can spend up to that run's caps and read that run's
own prompt, and nothing else. The worker still never sees repository
contents except through tool results the runner chose to return, which
the runner already returns today as the context pack.

The runner NetworkPolicy shrinks accordingly: DNS, the git remote over
the egress ports, Postgres, and the worker's gateway port. Nothing else.

### 2.3 Tools that need tenant state

Anything that needs the database, the embedder or the forge is a
gateway tool, not a runner tool: `search_index` in v1, and later the
review history of the same pull request or the findings of the previous
review. The gateway serves them on the same port under the same run
token, with the same caps. The rule: **file tools run in the sandbox,
stateful tools run in the worker, and the runner reaches nothing else.**

### 2.4 What a review records

`review_steps` rows, written by the runner role under its run id: step
number, role (model or tool), tool name and arguments, result size, the
tokens and cost the gateway reported for the step, and the elapsed time.
The context pack stays as the seed. Together they make every review
replayable: the harness can re-run a trajectory's tool calls against the
same commits and score both the answer and the path to it. Retention
follows `runner_runs`: the trajectory is kept, only tool result bodies
over a size cap are truncated.

### 2.5 Follow-ups

A follow-up (ADR-0002 §2.7) becomes the same agent with the thread, the
original findings and the original trajectory in its seed, run in a
runner pod like a review. Until that lands, follow-ups keep the
single-shot worker path built under ADR-0002.

### 2.6 Rollout and the gate

1. Gateway and per-run token in the worker; runner unchanged. The
   gateway is inert until a runner uses it.
2. Loop in the runner behind a per-tenant `review.loop` setting (`off`
   is today's single shot; `on` sets the caps), default off.
3. Harness mode `loop` alongside `diff` and `context`, scoring must-find
   recall, findings on expected ranges, cost and latency per mode, plus
   trajectory length.
4. The loop becomes the default when, on the corpus, it raises must-find
   recall by at least ten points over `context` at no more than three
   times the cost per review. If it does not, it stays opt-in and the
   ADR records the numbers.

## 3. Consequences

**Positive.** The reviewer can open what it needs instead of what a
heuristic guessed; the same sandbox can later run linters and tests; a
runaway loop is one pod with a deadline, not a worker slot; usage and
caps stay in one place; every review is replayable; single shot survives
as a mode and a baseline.

**Negative.** Reviews cost more and take longer, bounded by caps the
tenant sets. The runner image gains the model client (Fantasy's
OpenAI-compatible provider; the runner split into its own binary and
image, discussed under ADR-0002 §3.2, becomes more attractive). The
worker gains a listener and a token store. Tool results are untrusted
repository content in the prompt, as the context pack already is; the
findings stay anchored to diff lines and write-back stays in the worker.

**Unchanged.** Tenancy and row-level security, the queue and its
uniqueness rules, leases and caps, the forge surface, the chart's
topology, the index and its generations, metrics (two series are added:
loop steps and gateway calls, by tenant and model).

## 4. Alternatives considered

- **Loop in the worker, tools served by the runner.** The runner would
  expose read, list and grep over its checkout on an internal port and
  the worker would drive the model. Keeps the key where it is with no
  gateway, but every tool call is a network round trip, the worker holds
  a slot and a pod-to-pod connection for the whole loop, the agent code
  lands in the process that also owns write-back, and running a linter
  or a test in the sandbox would still need a second protocol. Rejected
  for the layout above, which has one protocol (the model API) and one
  place for accounting.
- **Loop in the runner with a provider key or a budgeted virtual key
  from a key-holding proxy such as LiteLLM.** Simplest code: the runner
  calls the provider directly. Rejected for v1 because it puts a
  credential that spends real money in the pod that runs untrusted
  content, and because per-tenant provider keys and caps would then live
  in the proxy rather than in kritik's own leases and usage rows. The
  worker gateway is that proxy, run by kritik, with the tenant model it
  already has. A deployment that already runs LiteLLM can point the
  worker's provider at it; the runner still only sees the gateway.
- **Keep single shot and widen retrieval.** Cheaper and deterministic,
  and still available as a mode. Rejected as the only mode because
  retrieval cannot ask a question the diff raises; the harness will say
  how much that costs.

## 5. Open questions

- Whether `search_index` should stay a gateway tool or the runner
  should receive the top-k similar chunks in its seed and go no further;
  the harness's ablation decides.
- The exact tool-result size and step defaults; the numbers above are
  starting points to be tuned on the corpus.
- Whether a follow-up needs the full loop or the seed plus a step or two.
