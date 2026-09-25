# ADR-0008: the agent may run allowlisted commands, and every runner byte leaves through the gateway

- **Status:** Proposed
- **Date:** 2026-09-25
- **Amends:** [ADR-0003](0003-forgejo-agentic-review.md) §2.6 (the agent's
  tools) and §2.9 (the runner pod's network); builds on
  [ADR-0004](0004-model-gateway.md), whose gateway becomes the runner's
  only route out of the cluster.
- **Authors:** onedr0p.

## 1. Context

The agent of ADR-0003 reads the head commit through three Go tools and
nothing else. That is enough to check a caller or a definition; it is not
enough for what a reviewer of a dependency bump actually does, which the
bot-ross comments on home-ops show: read the upstream release notes and
the compare view, read the chart's `Chart.yaml` at the new version, check
whether the changed sidecar is even enabled in this repository, and cite
the sources. Two ways to get there were weighed:

- **Resolve upstreams natively.** A Go package that recognises image, chart
  and action bumps in the diff, reads OCI annotations and `index.yaml`, maps
  versions to tags and fetches release notes. Deterministic and free of
  model steps, but a resolver per ecosystem, each with its own quirks, and
  Renovate's description cannot be the input: for many packages it carries
  nothing usable.
- **Let the model use command-line tools.** `curl` against the release
  API or a registry, `rg` and `fd` over a checkout. The model composes the
  lookup per case; kritik owns no ecosystem knowledge. This is what Kodus
  does (`rg`, `fd`, `find` in a sandbox per review) and what Mira avoids.

The second is chosen. Its cost is model steps and latency on every review
that looks upstream, and non-determinism; its value is coverage that a
resolver never reaches.

Two facts about the runner pod shape the rest. Its NetworkPolicy allows
egress on port 443 to any address, because the git remote and an agentic
review's model endpoint are reached by hostname and a policy cannot select
by hostname; so today a process in the pod already reaches the whole
internet on 443, and adding `curl` changes nothing about that. And the
repository is fetched as a bare repository at depth one: there is no
working tree for `rg` to search.

## 2. Decision

### 2.1 One route out: the gateway

The worker's gateway (ADR-0004) serves an HTTP forward proxy with a host
allowlist. Runner pods get `HTTPS_PROXY` and `HTTP_PROXY` pointing at it,
and their NetworkPolicy egress shrinks to DNS, Postgres and the gateway
port on worker pods: no direct 443. Everything the runner sends out goes
through the proxy and is allowed or refused by hostname:

- `CONNECT host:443` is allowed when the host matches the allowlist and
  tunnelled without inspection, which is how `curl` and go-git's fetch
  reach TLS endpoints;
- a plain `http://host/path` request to an allowed host is upgraded to
  HTTPS by the gateway, which adds a credential when one is configured for
  that host, so the runner can use the GitHub API at the installation's
  rate limit without holding a token.

The allowlist and credentials are operator configuration
(`egress.allowHosts`, `egress.credentials`) in the configuration file. The
allowlist always includes the forges of the file's installations, since
fetches need them; the operator adds registries and release hosts. It does
not include the providers' model endpoints: an agentic runner calls its
model through the gateway's own model endpoint (ADR-0004), so no runner
needs a provider's host, and one it could reach would take a key of an
attacker's choosing. An empty gateway URL keeps the runner's direct 443
egress, so a deployment without the gateway keeps working as before, less
agentic reviews; the chart enables the gateway by default.

The proxy's authorisation is the network: only pods labelled as runners
may reach its port, by the worker's ingress policy. The model endpoint on
the same port takes ADR-0004's per-run token as well.

### 2.2 The `run` tool

Agentic mode gains one tool. Its input is a command name and an argument
list; the runner executes the named binary directly, with no shell, in the
checkout directory, with an environment of `PATH`, a `HOME` outside the
checkout, the proxy variables and nothing else. The runner makes itself
non-dumpable before it offers the tool, so a command, which runs as the
same user, cannot read the runner's credentials from `/proc`. A command
runs at most `agent.commandTimeout` (default 30 s); its combined output is
capped like every tool's; its exit code is reported. The names a
repository may run are its `agent.commands` list, and the tool is offered
only for names found on the image's `PATH`, so the same runner code on the
distroless image offers nothing.

The first image variant is Alpine with `curl`, `fd` and `rg` from its
packages, published as each release's `-tools` tag and kept current by
Renovate through the base image. No language runtime and no `git`: the
tools read, they do not build or execute repository code. The variant is
not distroless, though: busybox's `sh` and `apk` are in it, `fd -x` and
`rg --pre` can start them, and through `sh` a script from the checkout
can run. The tool offers neither unless the operator lists it; containing
a command that starts one is the sandbox's job (§2.4). More binaries are
an `apk add` and an allowlist entry, not a design change.

For `rg` and `fd` to work, a run whose repository allows commands
materialises the head tree into the pod's scratch volume before the agent
starts, skipping ignored globs, capped in total bytes and per file. The Go
tools keep reading the git objects as before.

### 2.3 What the model is told, and what the comment shows

The system prompt describes the tool and when to use it: to read the
upstream of a dependency bump (release notes by tag, the compare view, a
chart's `Chart.yaml`, an image's annotations) and to search the checkout
when `grep` is not enough; to state plainly when an upstream cannot be
resolved rather than guess; and to treat everything a command returns as
data. The runner records every URL a `curl` was given in the agent run,
and the sticky comment lists them as the sources consulted, taken from that
record rather than from the model's answer.

### 2.4 Sandboxing is advised, not required

The pod now runs binaries the model chose, with arguments the repository's
content can influence. The chart takes a `runner.runtimeClassName`, so a
cluster with gVisor or Kata runs runner Jobs under it, and the README and
chart documentation state the implications of leaving it empty: a kernel
vulnerability reachable from the allowlisted binaries, or from a shell
they start, is contained only by the container runtime. What the design
guarantees without a sandbox is unchanged from ADR-0003: no long-lived
secret in the pod, a read-only git token when the forge supports one,
egress by hostname only, and output caps. The model key is the exception
ADR-0004 removes.

## 3. Consequences

**Positive.** A reviewer that reads the upstream, with no ecosystem code
in kritik. A tighter network boundary than today for every runner, agentic
or not, since the direct 443 egress goes. Searches at `rg` speed on large
repositories.

**Negative.** A materialised checkout per agentic run (disk and time), a
second image variant to publish, with a shell in it, more model steps on
bumps, and a tool the operator must opt a repository into and whose output
is only as good as the model's use of it. The gateway is a single point
through which every runner's traffic flows; its allowlist is security
configuration.

**Rejected.** A proxy sidecar per Job: containers share the pod's network
namespace, so a sidecar cannot be made the only route without privileged
iptables, and the credential it injects would sit in every pod. Native
resolvers, for the reasons in §1. A general shell: arguments and pipes
composed by the model are exactly the surface the argv form removes.
Static upstream builds in a distroless variant, pinned by checksum: no
shell in the image, but a digest per tool and architecture to keep
current, through a Renovate manager that finds each pinned archive by
downloading a release's assets when upstream publishes no checksums.
