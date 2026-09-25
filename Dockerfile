# syntax=docker/dockerfile:1

# ARGs used in a FROM must live in the global scope (before the first FROM).
# GO_VERSION is supplied by the release workflow from mise, the single source
# of truth for the toolchain (see .mise/config.toml).
ARG GO_VERSION

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

# Static, stripped, reproducible binary. GOARCH is left to the platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${REVISION}" \
    -o kritik ./cmd/kritik

# ---- Runner tools ---------------------------------------------------------
# curl, fd and rg for the agent's run tool (ADR-0008): static builds from
# their upstream releases, each archive checked against its pinned digest.
# Renovate moves a version and its digests together.
FROM golang:${GO_VERSION}-alpine AS tools-fetch
ARG TARGETARCH
# renovate: datasource=github-release-attachments depName=stunnel/static-curl
ARG CURL_VERSION=8.22.0
# renovate: datasource=github-release-attachments depName=stunnel/static-curl digestVersion=8.22.0
ARG CURL_AMD64_SHA256=dfb02460ba2abe513087538f12a3cf79b74b64a5ea3787ce8ac0cdb11251f884
# renovate: datasource=github-release-attachments depName=stunnel/static-curl digestVersion=8.22.0
ARG CURL_ARM64_SHA256=cf94cbeaae1b3c1944a4a761ef04478f8f0b23e93a324b5ed009b2082eda11f3
# renovate: datasource=github-release-attachments depName=sharkdp/fd
ARG FD_VERSION=v10.5.0
# renovate: datasource=github-release-attachments depName=sharkdp/fd digestVersion=v10.5.0
ARG FD_AMD64_SHA256=761c72dc8e120d85b22292063be8a796e2eeb20eb3e4f38b8fa2343ccf3514a7
# renovate: datasource=github-release-attachments depName=sharkdp/fd digestVersion=v10.5.0
ARG FD_ARM64_SHA256=d76c4317f7d5dba69f8a2a15856c90c777e7f0dd4e85f0de8c76de6992c374d4
# renovate: datasource=github-release-attachments depName=BurntSushi/ripgrep
ARG RG_VERSION=15.2.0
# renovate: datasource=github-release-attachments depName=BurntSushi/ripgrep digestVersion=15.2.0
ARG RG_AMD64_SHA256=33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c
# renovate: datasource=github-release-attachments depName=BurntSushi/ripgrep digestVersion=15.2.0
ARG RG_ARM64_SHA256=800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915

WORKDIR /tools
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64 curl_sha=${CURL_AMD64_SHA256} fd_sha=${FD_AMD64_SHA256} rg_sha=${RG_AMD64_SHA256} ;; \
      arm64) arch=aarch64 curl_sha=${CURL_ARM64_SHA256} fd_sha=${FD_ARM64_SHA256} rg_sha=${RG_ARM64_SHA256} ;; \
      *) echo "no runner tools for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    fetch() { wget -qO "$1" "$2" && echo "$3  $1" | sha256sum -c -; }; \
    fetch curl.tar.xz "https://github.com/stunnel/static-curl/releases/download/${CURL_VERSION}/curl-linux-${arch}-musl-${CURL_VERSION}.tar.xz" "${curl_sha}"; \
    fetch fd.tar.gz "https://github.com/sharkdp/fd/releases/download/${FD_VERSION}/fd-${FD_VERSION}-${arch}-unknown-linux-musl.tar.gz" "${fd_sha}"; \
    fetch rg.tar.gz "https://github.com/BurntSushi/ripgrep/releases/download/${RG_VERSION}/ripgrep-${RG_VERSION}-${arch}-unknown-linux-musl.tar.gz" "${rg_sha}"; \
    tar -xJf curl.tar.xz curl; \
    tar -xzf fd.tar.gz --strip-components=1 "fd-${FD_VERSION}-${arch}-unknown-linux-musl/fd"; \
    tar -xzf rg.tar.gz --strip-components=1 "ripgrep-${RG_VERSION}-${arch}-unknown-linux-musl/rg"; \
    rm curl.tar.xz fd.tar.gz rg.tar.gz

# ---- Runtime with runner tools ----------------------------------------------
# kritik with the run tool's commands on PATH, for runner Jobs through the
# chart's runner.image. Built with --target tools; published as the -tools
# tag of each release.
FROM gcr.io/distroless/static:nonroot AS tools
WORKDIR /
COPY --from=builder /workspace/kritik /kritik
COPY --from=tools-fetch --chown=0:0 /tools/ /usr/local/bin/
EXPOSE 8080 8081
ENTRYPOINT ["/kritik"]

# ---- Runtime --------------------------------------------------------------
# The default target, last so a build without --target produces it.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/kritik /kritik
EXPOSE 8080 8081
ENTRYPOINT ["/kritik"]
