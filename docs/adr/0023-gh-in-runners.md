# ADR-0023: gh in runners, with a read-only token for the run

- **Status:** Proposed
- **Date:** 2026-09-30
- **Authors:** onedr0p.
- **Amends:** [ADR-0008](0008-runner-tools.md) §2.1 (the runner uses
  the GitHub API without holding a token) and
  [ADR-0011](0011-runner-tool-images.md) §2.1 (the stock tools).

> Scope: how an agentic review reads GitHub from its runner. The run
> tool, the gateway and the sources a review lists are otherwise
> unchanged.

## 1. Context

An agentic review reads a dependency bump's upstream, its release notes,
its compare view and its files, mostly on GitHub. It does so with `curl`
through the gateway, which authenticates to a host only on a plain
`http://` request it can see into. The agent writes raw API URLs, pages
through JSON by hand, and runs into the unauthenticated rate limit unless
an admin adds a credential for `api.github.com`.

`gh` reads the same resources in one call each (`gh release view`,
`gh api .../compare/...`), but it only talks HTTPS, which passes the
gateway as an opaque tunnel, and it refuses to run without a token. So
`gh` needs the runner to hold a token itself.

The runner already holds one: the installation token it fetches the
checkout with. It was the worker's own, cached token, with every
permission the App has, across every repository of the installation.

## 2. Decision

### 2.1 The runner's token is read-only and for one repository

The worker mints a token for each run with only `contents: read` and
`metadata: read`, for the repository under review alone, and hands the
runner that token instead of its own. The runner fetches with it, as
before; nothing else it does needs more. A GitHub App keeps read access to
public repositories, so the token also reads the upstreams a review
consults.

### 2.2 gh is in the tools image, and signs in with that token

The `-tools` image adds `gh`. When `gh` is among an agentic repository's
`agent.commands`, the run tool gives it, and only it, `GH_TOKEN` set to
the run's token, with prompts and update checks off. The gateway allows
`api.github.com` wherever it allows `github.com`. The run tool's
description tells the model to use `gh`, not `curl`, for anything on
GitHub, with examples.

### 2.3 What gh reads is a source

A `gh api` call's resource, and the release, pull request, issue or
repository a `gh ... view` names, is recorded as a source, like a URL
`curl` is given, and the review's comment links it through
`redirect.github.com`.

## 3. Consequences

- An agent's commands can use the run's token. It reads only what the
  runner already has checked out and public repositories, and expires
  within the hour.
- GitHub lookups are authenticated, so they count against the
  installation's rate limit rather than the gateway address's anonymous
  one.
- `curl` stays for everything off GitHub, through `egress.allowHosts`.

## 4. Rejected alternatives

- **A credential the gateway adds, as for `curl`.** The gateway cannot
  see into an HTTPS tunnel; intercepting TLS would mean a certificate
  authority that every runner trusts.
- **A second token for `gh` alone.** Two tokens with the same reach, and
  the checkout's token would keep permissions it never uses.
