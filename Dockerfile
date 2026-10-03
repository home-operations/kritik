# syntax=docker/dockerfile:1

# ARGs used in a FROM must live in the global scope (before the first FROM).
# GO_VERSION and NODE_VERSION are supplied by the release workflow from mise,
# the single source of truth for the toolchain (see .mise/config.toml).
ARG GO_VERSION
ARG NODE_VERSION

# ---- UI build ---------------------------------------------------------------
# The built UI is the same bytes on every platform, so it is built once, on
# the build host, rather than per target under emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/node:${NODE_VERSION}-trixie-slim AS ui
WORKDIR /ui
COPY internal/web/package.json internal/web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY internal/web/ ./
RUN npm run build

# ---- Go build -------------------------------------------------------------
FROM docker.io/library/golang:${GO_VERSION}-trixie AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=dev

WORKDIR /workspace
# Cache module downloads before copying source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=ui /ui/dist/ internal/web/dist/

# Static, stripped, reproducible binary. GOARCH is left to the platform.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${REVISION}" \
    -o kritika ./cmd/kritika

# ---- Runner tools -----------------------------------------------------------
# The release builds of the tools the -tools image puts on PATH, installed by
# mise from build/tools/mise.toml at the URLs and checksums its lockfile pins
# for this platform. A glibc stage, so mise records the same linux-x64 and
# linux-arm64 platforms as the repository's own lockfile. mise's image has no
# shell to copy the binaries out with, so its static binary and the CA roots
# it downloads with are taken from it instead.
FROM docker.io/jdxcode/mise:2026.10.1 AS mise
FROM docker.io/library/debian:trixie-slim AS runner-tools
COPY --from=mise /usr/local/bin/mise /usr/local/bin/mise
COPY --from=mise /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /tools
COPY build/tools/mise.toml build/tools/mise.lock ./
# The manifest keeps each tool's bin path down to the executables it is
# there for, so the bin paths are copied whole.
RUN mise trust && mise install --locked \
    && mkdir /out && mise bin-paths | xargs -I{} cp -rL {}/. /out/

# ---- Runtime with runner tools ----------------------------------------------
# kritika with the agent's commands on PATH for its run tool, the chart's
# runner image for runner Jobs. Built with --target tools; published as the
# -tools tag of each release. Distroless with glibc, libgcc and libstdc++ for
# the tools' release builds, and no shell or package manager, so those
# commands are the only other programs a pod can start.
FROM gcr.io/distroless/cc:nonroot AS tools
COPY --from=runner-tools /out/ /usr/local/bin/
COPY --from=builder /workspace/kritika /kritika
ENTRYPOINT ["/kritika"]

# ---- Runtime --------------------------------------------------------------
# The default target, last so a build without --target produces it.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/kritika /kritika
ENTRYPOINT ["/kritika"]
