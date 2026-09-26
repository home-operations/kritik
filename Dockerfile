# syntax=docker/dockerfile:1

# ARGs used in a FROM must live in the global scope (before the first FROM).
# GO_VERSION and NODE_VERSION are supplied by the release workflow from mise,
# the single source of truth for the toolchain (see .mise/config.toml).
ARG GO_VERSION
ARG NODE_VERSION

# ---- UI build ---------------------------------------------------------------
# The built UI is the same bytes on every platform, so it is built once, on
# the build host, rather than per target under emulation.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS ui
WORKDIR /ui
COPY internal/web/package.json internal/web/package-lock.json ./
RUN npm ci
COPY internal/web/ ./
RUN npm run build

# ---- Go build -------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=dev

WORKDIR /workspace
# Cache module downloads before copying source.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=ui /ui/dist/ internal/web/dist/

# Static, stripped, reproducible binary. GOARCH is left to the platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${REVISION}" \
    -o kritik ./cmd/kritik

# ---- Runtime with runner tools ----------------------------------------------
# kritik with curl, fd and rg on PATH for the agent's run tool (ADR-0008),
# for runner Jobs through the chart's runner.image. Built with --target
# tools; published as the -tools tag of each release.
FROM alpine:3.24 AS tools
RUN apk add --no-cache curl fd ripgrep
COPY --from=builder /workspace/kritik /kritik
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/kritik"]

# ---- Runtime --------------------------------------------------------------
# The default target, last so a build without --target produces it.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/kritik /kritik
EXPOSE 8080 8081
ENTRYPOINT ["/kritik"]
