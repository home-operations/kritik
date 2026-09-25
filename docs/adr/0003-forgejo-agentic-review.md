# ADR-0003: Forgejo, agentic reviews, in-repo configuration and a templated contract

- **Status:** Proposed
- **Date:** 2026-09-24
- **Amends:** [ADR-0002](0002-kritik-pr-review-service.md) §2.6, §2.7, §2.11 and §2.12.

## 1. Context

ADR-0002 shipped GitHub, a single structured completion per review, a
hard-coded system prompt, a fixed findings schema and OpenRouter as the only
provider. Organisations that already review pull requests with a CI-hosted
agent on a self-hosted Forgejo cannot move to kritik, because it lacks:

- a Forgejo forge client (webhooks parse, but `BuildForge` refuses Forgejo);
- review rules the repository owns, read from a ref the pull request cannot
  rewrite;
- an output contract with a required fix and severities a reviewer acts on,
  and control over how it is rendered;
- the pull request body and changed paths as skip criteria;
- re-reviews that look at what changed since the last review instead of the
  whole pull request again;
- a reviewer that can read the repository beyond the precomputed context
  pack when a finding depends on code the pack did not include;
- direct Anthropic and OpenAI endpoints, and gateways compatible with either.

## 2. Decision

### 2.1 Forgejo

A typed `net/http` client in `internal/forge/forgejo` implements
`forge.Client` against `/api/v1` with `Authorization: token`. It is not built
on the Forgejo SDK, which binds a context to the client instead of each call.
Inline findings are a pull review with event `COMMENT` and comments carrying
`path` and `new_position`; the status is a commit status with context
`kritik/review`; the bot is `GET /user`. `MergeBase` gains the pull request
number so Forgejo can read the pull request's `merge_base`. Collaborator
permissions normalise onto a typed `forge.Permission`. The poller serves every
forge with a client. Forgejo's `synchronized` action is normalised to
`synchronize`.

Forgejo has no short-lived installation token: `GitToken`, the credential a
runner fetches with, is a static token. A Forgejo (and later GitLab)
installation takes an optional `gitToken` secret reference, resolved and
validated like `token`, and runners receive it in place of `token` when it is
set. `token` must write comments, reviews and statuses; `gitToken` should be a
separate read-only token, so the pod that reads untrusted content (§2.6)
cannot write to the forge. Without it the API token reaches the runner.

### 2.2 Filter inputs

`pr.body` joins the CEL filter's variables, the stored pull request row, and
the prompt (as untrusted data).

### 2.3 `.kritik.yaml`

The runner reads `.kritik.yaml` and every file it names from the **merge-base
tree**, which is base-branch history the pull request cannot change, caps each
file at 256 KiB and all of them at 1 MiB, and stores them with the context
pack. The worker decodes it strictly and merges it onto the operator's
settings. It may only narrow what the operator allows: its `filter` is ANDed
with the operator's, `enabled: false` disables, `ignore` adds globs. It may
also set presentation and strictness, which grant nothing:

```yaml
enabled: true
filter: '!pr.body.contains("[skip-review]")'
ignore: ["web/src/generated/**"]
skip:
  onlyPaths: ["docs/**", "**/README.md", "**/CHANGELOG.md"]
review:
  instructions: [".kritik/rules.md"]
  requireSuggestedFix: true
  templates:
    summary: ".kritik/summary.md.j2"
    inline: ".kritik/inline.md.j2"
```

The in-repo filter and `skip.onlyPaths` (skip when every changed path
matches) are evaluated after the runner, since only the runner has the tree
and the changed paths. A skipped review posts a success status saying why.

Operator-only repository keys are `mode` (`single`, the default, or
`agentic`), `agent` (`maxSteps`, `maxToolOutputBytes`, `maxTokens`, `timeout`),
`incremental.maxDeltaFiles` (default 25), `settle` (a duration) and a
`review` block of defaults the in-repo file may override.

### 2.4 The contract

One typed contract replaces the old findings schema:

```go
type Severity string // blocking | important | nit
type Summary struct{ Take string; Praise []string } // at most three praise items
type Finding struct {
    Path string; Line int; Severity Severity
    Title, Explanation, SuggestedFix string
}
type Result struct{ Summary Summary; Findings []Finding }
```

The JSON schema the model fills is derived from these types. `SuggestedFix`
is optional unless `requireSuggestedFix` is set, in which case a finding
without one is dropped and counted. Existing rows map error → blocking,
warning → important, info → nit.

Rendering is [gonja](https://github.com/nikolalohinski/gonja) (Jinja2)
templates. kritik embeds defaults for the summary and the inline comment; a
repository may replace either. Repository templates run with no loader, so
`include`, `import` and `extends` resolve nothing, under an output cap of
64 KiB and a render deadline. A failing template falls back to the default
and the summary says so. kritik prepends its own marker to the sticky comment,
so no template can hide it from sticky discovery.

### 2.5 Providers

Fantasy is removed. `internal/model` defines typed messages, tool
definitions, tool calls and usage, and a `Stepper` that performs one model
turn. `Completer` (a forced tool call returning the contract) is built on it.
Adapters use the vendors' official SDKs:

| Type         | SDK                                      | Notes                                                                                |
| ------------ | ---------------------------------------- | ------------------------------------------------------------------------------------ |
| `anthropic`  | `github.com/anthropics/anthropic-sdk-go` | Prompt caching on the system prompt and the rolling last message                     |
| `openai`     | `github.com/openai/openai-go/v3`         | Chat completions                                                                     |
| `openrouter` | `github.com/openai/openai-go/v3`         | The OpenAI adapter at OpenRouter's URL, with its `models` fallback and reported cost |

Every type accepts `baseUrl`, so any gateway compatible with either API is a
provider. OpenRouter reports dollar cost; for the others an optional typed
per-model `pricing` block (USD per million input, output, cache-read and
cache-write tokens) computes it, and without one cost is recorded as zero
while tokens still count against caps.

### 2.6 Agentic mode

In `agentic` mode the runner Job owns the whole review: it fetches, builds the
context pack, then runs a bounded tool loop and writes the result. The tools
read the head commit's git objects directly, never a working tree, so there
is no path or symlink escape:

- `read_file` (path and line range),
- `grep` (RE2, so pattern cost is linear; binary and ignored files skipped),
- `list_files` (glob),
- `submit_review` (the contract; forced when the step or token budget is
  nearly spent).

Every tool's output is capped. The loop ends on `submit_review`, `maxSteps`,
the token budget or the Job deadline, and writes an `agent_runs` row with the
result, the stop reason, steps, a tool histogram and usage. A run cancelled
from outside (superseded, heartbeat lost, deadline) still writes its row, with
stop `canceled` and the usage so far, and the worker charges it.

The token budget is the repository's `agent.maxTokens` (4M by default), cut
to what is left of the tenant's `tokensPerMonth` when one is set; with less
than a small floor left the review ends `capped` before the Job starts.

The worker keeps everything that writes outward or spends against limits: it
checks caps, takes the model lease and renews it for the Job's lifetime,
spawns the Job with the provider type, base URL, models and key, and after
the Job anchors, renders, posts and persists exactly as in single mode.

This puts a model key into the pod that reads untrusted content. The pod runs
no repository code, the model's tools are read-only, and its only output is
findings text the worker validates, so a prompt injection can at worst shape
that text. The key reaches the pod as described in §2.9. Operators should give
each tenant its own key with a spending limit, and on Forgejo a read-only
`gitToken` (§2.1), since the git token reaches the same pod.

Runner Jobs run in the worker's namespace, and the worker's Role can create,
patch and delete every Secret in it, which a Role cannot narrow to the run
Secrets it names only at runtime. kritik should therefore get a namespace
of its own, holding no Secrets but its own.

### 2.7 Incremental re-review

The worker finds the pull request's last completed review. The runner fetches
that review's head as a third commit. If it cannot (a force-push made it
unreachable) or at least `maxDeltaFiles` files changed since, the review is a
full one and records why. Otherwise the prompt adds the diff since the last
review and the previous findings, with the instruction to verify each and
report again those still unfixed. The full merge-base diff still decides
which lines a finding may anchor to. A finding whose fingerprint (path and
normalised title) matches one already posted inline is listed in the summary
but not posted inline again.

### 2.8 Settle window

A repository's `settle` delays a review job for a new head. A push inside the
window supersedes it, and the existing head check at the start of the job
discards the stale one before anything is spent.

### 2.9 The worker–runner protocol

The relationship between a worker and a runner Job follows the shape of CI
job runners such as GitHub Actions' runner: one complete job document in,
job-scoped secrets delivered and masked separately, a heartbeat the
dispatcher watches, cancellation when the work is superseded, and structured
results reported back.

- **Job document.** The worker hands the runner one typed, versioned
  `runner.Spec` (`version: 1`) as JSON: run kind and id, clone URL, head,
  merge-base and prior head, ignore globs, review mode, agent limits and the
  model endpoint (provider, base URL, models, pricing). In agentic mode it
  also carries a prompt block: the pull request's metadata and text (the
  fields the repository filter sees), the operator's instruction paths and
  strictness, and the last completed review's findings. The runner applies
  the merge-base `.kritik.yaml` with the same code as the worker and does not
  run the agent for a review the worker will skip. It holds no secret.
  The runner rejects a version it does not know, or a field it does not
  know, rather than guessing.
- **Transport and size.** The document is a key (`run-spec.json`) of the
  run's Secret, mounted read-only into the pod at
  `/var/run/kritik/spec.json`, whose path `KRITIK_RUN_SPEC_FILE` names. An
  environment variable was ruled out: Linux caps one environment string at
  128 KiB and JSON escaping can grow a body sixfold, so a large pull request
  would fail the Job on every retry with `E2BIG`. A Secret is capped at
  1 MiB with the credentials beside it, so the encoded document is refused
  above 900 KiB, and the worker cuts what a pull request grows without
  bound first: the body to 64 KiB at a rune boundary, and the prior
  findings to 200, halved further while the prompt is still over its
  share.
- **Job-scoped secrets.** The git token, in agentic mode the model key,
  and the job document go into a Secret created for the run and owned by its Job, so Kubernetes
  deletes it with the Job. The worker creates the Secret, then the Job, then
  sets the Secret's owner reference; a failure deletes the Secret. The worker
  masks those values out of the log tail it stores.
- **Heartbeat.** The runner stamps `runner_runs.heartbeat_at` while it
  works. The worker treats a run whose heartbeat is older than 90 seconds
  after start as dead and ends it, instead of waiting out the Job deadline.
- **Cancellation.** While it waits, the worker checks the pull request's
  head. When a newer head arrives it deletes the Job (foreground
  propagation, so the pod goes too) and records the review as superseded;
  the newer head has its own job queued. Cancelling the worker's context
  deletes the Job the same way.
- **Results.** The runner reports through the runner database role only:
  the context pack, and in agentic mode an `agent_runs` row with the result,
  stop reason, usage and a per-step timeline (tool, duration, bytes, tokens).
  It never writes to the forge.

## 3. Consequences

- The findings schema, severities and the provider configuration change
  incompatibly; kritik has not had a release that would make this costly.
- Forgejo reaches the same acceptance list as GitHub in ADR-0002 §2.7.
- Agentic reviews cost more per review and run longer; they are opt-in per
  repository in the operator's file only.

## 4. Deferred

- Retrieval from an external memory service, sub-agent fan-out, and depth
  tiers keyed on pull request size.
- A follow-up responder that edits code.
- The GitLab client.
- In agentic mode, a fallback model on another provider (the runner holds
  one provider's key) and similar-code context from the index (the agent
  greps instead).
