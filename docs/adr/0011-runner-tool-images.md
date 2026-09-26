# ADR-0011: the agent's commands may come from image volumes

- **Status:** Proposed
- **Date:** 2026-09-26
- **Amends:** [ADR-0008](0008-runner-tools.md) §2.2 (where the `run`
  tool's binaries come from).
- **Authors:** onedr0p.

> Scope: how an operator adds command-line tools, such as `helm` or
> `flate`, to the agent's `run` tool without building a runner image, and
> which runs get them. It does not change the `run` tool itself, its
> allowlist, or its route out through the gateway.

## 1. Context

ADR-0008 §2.2 gives the agent a `run` tool that executes the commands a
repository's `agent.commands` allows, offered only for names found on the
runner image's `PATH`. The `-tools` image carries `curl`, `fd` and `rg`;
anything else means building and maintaining another runner image, and
every repository on that image then carries every tool in it.

Kubernetes can mount an OCI image as a volume (`VolumeSource.Image`): the
image's layers merged into one read-only directory in the pod, with
`subPath` mounts supported from 1.33 (per the field's documentation in
`k8s.io/api` v0.37.1). A probe pod on a v1.37.1 cluster with containerd
2.3.5, under the gVisor runtime class and with the runner pod's security
context (non-root, read-only root filesystem, every capability dropped),
ran a static binary from a whole-image mount, ran `rg` from a `subPath`
mount put on `PATH`, and was refused a write into the mount.

## 2. Decision

### 2.1 Stock tools stay in the runner image

`curl`, `fd` and `rg` are supported out of the box, from the `-tools` image
as ADR-0008 §2.2 describes. Nothing about them changes.

### 2.2 More tools come from a `tools` catalog in the file

```yaml
tools:
  - name: helm
    image: ghcr.io/example/helm:3.19.0@sha256:...
    path: /usr/local/bin
  - name: flate
    image: ghcr.io/example/flate:1.4.0@sha256:...
    commands: [flate]
```

- `name` names the tool and its pod volume: lowercase alphanumerics and
  hyphens, at most 58 characters, unique.
- `image` is the image the tool comes from, pulled the way the runner image
  is (node credentials and the pod's pull secrets). Pinning it by digest is
  advised.
- `path` is the directory inside the image that holds the binaries,
  default `/`; a clean absolute path.
- `commands` are the binaries the tool provides, the names `agent.commands`
  allows, default the tool's name. No two tools may provide the same
  command. A tool may provide a stock command's name, and then comes first.

The catalog is instance scope, in the file only (ADR-0010 §2.3): which code
runs next to untrusted pull requests is the operator's decision. A
dashboard tenant or a repository's `.kritik.yaml` can allow a tool's
command only where the operator's allowlist already does; neither can add
a tool.

### 2.3 A run mounts only the tools it may use

An agentic review mounts the tools that provide at least one command in
its effective `agent.commands`. A single-shot review and an index run
mount none; neither runs commands.

### 2.4 Mount layout and `PATH`

Each tool is an image volume mounted read-only at
`/opt/kritik/tools/<name>`, from `path` as its `subPath`. The runner
container's `PATH` is the tool directories, in catalog order, ahead of the
`PATH` both runner images set (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`),
since a container's `PATH` variable replaces the image's rather than
extending it. The runner's existing lookup then offers a tool's commands
like the stock ones, and says "not offered" for one its image lacks. The
runner and its job document do not change.

### 2.5 What a tool image must be

- **Statically linked.** The default runner image is distroless, with no C
  library; the `-tools` image has musl. A Go binary built without cgo fits
  both.
- **Reachable only through the gateway once running.** A tool runs under
  the `run` tool, with the proxy variables set, so its network access is the
  gateway's allowlist (ADR-0008 §2.1), like `curl`'s.
- **On a cluster that supports image volumes, and pullable.** Where image
  volumes are unavailable or a tool's image cannot be pulled, the pod does
  not start: the kubelet retries with backoff and reports why on the pod,
  and the run fails when the Job's deadline ends it.

The local executor has no images to mount and leaves tools out.

## 3. Consequences

- **Operators add a tool with a file change** and a reload, without
  building or rolling a runner image, and only the repositories that allow
  its command pull and mount it.
- **Every catalog entry is code the agent can run** inside the runner, next
  to untrusted content. The catalog, the per-repository allowlist and the
  egress allowlist bound it; the sandboxing ADR-0008 §2.4 advises matters
  more with every tool added.
- **The first run on a node pulls the tool's image**, which delays that
  run's start.

## 4. Rejected alternatives

- **A runner image per set of tools.** Every combination is another image to
  build and keep current, and every repository on it carries every tool.
- **An init container copying binaries into a shared volume.** It works on
  older clusters, but leaves executables in a writable volume the agent's
  commands share and costs a copy per run.
- **Downloading tools at run time.** It needs egress to download hosts and
  its own integrity checks, and the agent could fetch anything else the
  same way.
- **Tool catalogs per tenant or per repository.** The code that runs beside
  untrusted pull requests is the operator's call.
