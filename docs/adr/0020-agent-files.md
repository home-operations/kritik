# ADR-0020: a repository's AGENTS.md and CLAUDE.md join the instructions

- **Status:** Proposed
- **Date:** 2026-09-29
- **Authors:** onedr0p.
- **Amends:** [ADR-0010](0010-configuration-layers.md) §2.4 (one more
  review key).

> Scope: which files a review reads for the guidance a repository already
> writes for coding agents, where they go in the prompt, and the switch.

## 1. Context

A review follows the instruction files `review.instructions` names, so a
repository that wants its conventions checked writes them down and names
them, in the configuration or its `.kritik.yaml`. Many repositories have
already written them down for coding agents: an `AGENTS.md` at the root,
more in the directories that differ, and a `CLAUDE.md` that is often only
`@AGENTS.md`. Those files say how the code is laid out, how errors are
handled and what not to do, which is what a reviewer needs too, and
nobody names them to kritik.

## 2. Decision

### 2.1 Which files

The runner reads, from the merge base, the `AGENTS.md` of the repository
root and of every directory above a changed path, and a directory's
`CLAUDE.md` where it has no `AGENTS.md`. A change to `svc/api/handler.go`
reads `AGENTS.md`, `svc/AGENTS.md` and `svc/api/AGENTS.md`, whichever
exist, so a nested file applies to the changes under it and to no
others. A directory without either file is not noted; one over the
per-file limit, or past the total, is noted like a named file.

`CLAUDE.md` is only a fallback because most repositories that have both
keep one in the other, and reading both would say everything twice.
Imports (`@path`) are not followed.

### 2.2 Where they go

They follow the named instruction files in the prompt's repository
instructions, shallowest first, and share their 32 KiB cap, so a
repository's review-specific instructions win the room. A file the
instructions already name is not added twice. Single and agentic reviews
get them alike. A follow-up takes the ones its pull request's last review
read, from its context pack, rather than reading each directory again
through the forge's API.

### 2.3 The switch

`review.agentFiles`, on unless set, turns it off at the defaults, an
account, a repository entry or the repository's `.kritik.yaml`, which may
replace the admin's value: the files are the repository's own, and
leaving them out loosens nothing an admin set.

## 3. Consequences

- A repository with an `AGENTS.md` is reviewed against it with no
  configuration.
- Guidance written for agents that write code, such as how to run the
  tests or when to commit, reaches a reviewer that does neither; the
  prompt already tells it that instructions refine what to look for and
  nothing else.
- The files a review read are listed on its Raw tab with the other
  repository files; the Rules page does not list them.

## 4. Rejected alternatives

- **Every AGENTS.md in the repository.** A file for another part of the
  tree speaks to changes the review does not have, and spends the cap.
- **Name them as context files.** An agentic review is only pointed at a
  context file, so the guidance would depend on the agent opening it.
- **Off unless set.** Few would find the switch, and the files exist to
  be read by whatever works on the repository.
